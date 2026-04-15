package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/contactutil"
	"github.com/shridarpatil/whatomate/internal/integrations/crm"
	"github.com/shridarpatil/whatomate/internal/integrations/telnyx"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/internal/websocket"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// TelnyxWebhookHandler is the public endpoint Telnyx posts call events to.
//
// Phase 2.2: full dispatcher.
//
// Pipeline for every incoming webhook:
//
//	1. Parse envelope, extract event type
//	2. Look up TelnyxConnection by call_control_app_id
//	3. Verify Ed25519 signature against the connection's public key
//	4. Dispatch by event type:
//	      call.initiated         → create CallLog, run IVR entry node
//	      call.answered          → mark CallLog answered
//	      call.playback.ended    → AdvanceTelnyxIVR("default")
//	      call.gather.ended      → AdvanceTelnyxIVR("digit:N" or "timeout")
//	      call.bridged           → log transfer success
//	      call.hangup            → finalize CallLog, fire missed-call fallback
//	      call.recording.saved   → enqueue download of the recording mp3
//	      anything else          → log+200 (do not retry)
//
// All steps are best-effort wrt the response: we always return 2xx unless
// the signature was invalid (403). This prevents Telnyx from spamming
// retries on transient errors that the operator should fix manually.
//
// Route: POST /api/webhook/telnyx (public, rate-limited).
func (a *App) TelnyxWebhookHandler(r *fastglue.Request) error {
	body := r.RequestCtx.PostBody()

	var env telnyx.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		a.Log.Warn("Telnyx webhook: invalid JSON", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid JSON", nil, "")
	}

	// Extract a generic call payload (every call.* event has these fields)
	// to know which connection_id sent it.
	callPayload, parseErr := telnyx.ParseCallPayload(&env)
	if parseErr != nil {
		// Non-call event (account.balance, etc.) — log + 200 + bail.
		a.Log.Debug("Telnyx webhook: non-call event ignored",
			"event_type", env.Data.EventType,
			"id", env.Data.ID)
		return r.SendEnvelope(map[string]string{"status": "ignored"})
	}

	// Look up the iReparo connection by call_control_app_id.
	var connection models.TelnyxConnection
	if err := a.DB.
		Where("call_control_app_id = ?", callPayload.ConnectionID).
		First(&connection).Error; err != nil {
		a.Log.Warn("Telnyx webhook: no connection registered",
			"call_control_app_id", callPayload.ConnectionID,
			"event_type", env.Data.EventType)
		return r.SendEnvelope(map[string]string{"status": "unknown_connection"})
	}
	connection.DecryptSecrets(a.Config.App.EncryptionKey)

	// Signature verification.
	signature := string(r.RequestCtx.Request.Header.Peek(telnyx.HeaderSignature))
	timestamp := string(r.RequestCtx.Request.Header.Peek(telnyx.HeaderTimestamp))
	if connection.PublicKey == "" {
		a.Log.Warn("Telnyx webhook: signature NOT verified — public key not configured for connection. Set it in Settings.",
			"connection_id", connection.ID,
			"organization_id", connection.OrganizationID)
	} else if err := telnyx.VerifyWebhook(connection.PublicKey, signature, timestamp, body); err != nil {
		a.Log.Warn("Telnyx webhook: signature verification failed — rejecting",
			"connection_id", connection.ID,
			"error", err)
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Invalid signature", nil, "")
	}

	// Build the dependency bundle the dispatcher functions need.
	tnxClient := telnyx.NewClient(connection.APIKey, a.HTTPClient)

	// Derive the publicly reachable base URL from this incoming webhook
	// request. Telnyx is POSTing to the Host it was configured with, so
	// reusing it guarantees the signed audio URLs we hand back to Telnyx
	// resolve to the same instance it already knows about.
	scheme := "https"
	if !r.RequestCtx.IsTLS() && string(r.RequestCtx.Request.Header.Peek("X-Forwarded-Proto")) != "https" {
		scheme = "http"
	}
	host := string(r.RequestCtx.Host())
	baseURL := scheme + "://" + host

	deps := &calling.TelnyxIVRDeps{
		DB:     a.DB,
		Telnyx: tnxClient,
		AudioURLResolver: func(filename string) string {
			return a.BuildSignedIVRAudioURL(baseURL, filename)
		},
	}

	// Dispatch by event type. We log the dispatch error but always 200
	// the webhook to avoid retry storms.
	if err := a.dispatchTelnyxEvent(r.RequestCtx, deps, &connection, &env, callPayload, body); err != nil {
		a.Log.Warn("Telnyx webhook: dispatch error",
			"event_type", env.Data.EventType,
			"call_control_id", callPayload.CallControlID,
			"error", err)
	}
	return r.SendEnvelope(map[string]string{"status": "accepted"})
}

// dispatchTelnyxEvent routes the event to the right handler. Returns an
// error for logging purposes only — the HTTP response is always 200.
func (a *App) dispatchTelnyxEvent(
	rctx *fasthttp.RequestCtx,
	deps *calling.TelnyxIVRDeps,
	connection *models.TelnyxConnection,
	env *telnyx.Envelope,
	payload *telnyx.CallEventPayload,
	rawBody []byte,
) error {
	// Background context for our outbound API calls so the response to
	// Telnyx returns quickly. We give each command a 10s budget.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch env.Data.EventType {

	// ---- Inbound call lifecycle ---------------------------------------

	case telnyx.EventCallInitiated:
		// For click-to-call (outbound) flows, we already created the
		// CallLog at Dial time. Skip the inbound-style handler for these.
		if state, _ := DecodeClickToCallState(payload.ClientState); state != nil {
			return nil
		}
		return a.handleTelnyxCallInitiated(ctx, deps, connection, payload)

	case telnyx.EventCallAnswered:
		// Click-to-call: the agent leg just answered. Trigger the Transfer
		// to the customer instead of treating it as an IVR answer.
		if handled, err := a.handleClickToCallAgentAnswered(ctx, connection, payload.CallControlID, payload.ClientState); handled {
			return err
		}
		return a.handleTelnyxCallAnswered(ctx, deps, connection, payload)

	// ---- IVR continuation ---------------------------------------------

	case telnyx.EventPlaybackEnded:
		return calling.AdvanceTelnyxIVR(ctx, deps, payload.CallControlID, payload.ClientState, "default")

	case telnyx.EventGatherEnded:
		// Decode the digits from the payload. The continuation routes
		// differently depending on whether the current node is a `menu`
		// (digit: edge) or a `gather` (default edge + variable storage),
		// so we hand off to the specialized AdvanceTelnyxIVRAfterGather.
		var gatherPayload struct {
			Digits string `json:"digits"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(env.Data.Payload, &gatherPayload)
		return calling.AdvanceTelnyxIVRAfterGather(ctx, deps, payload.CallControlID, payload.ClientState, gatherPayload.Digits)

	// ---- Transfer / bridge --------------------------------------------

	case telnyx.EventCallBridged:
		// Two legs joined (transfer succeeded). Log it on the CallLog.
		a.markTelnyxBridged(connection.OrganizationID, payload)
		return nil

	// ---- Hangup -------------------------------------------------------

	case telnyx.EventCallHangup:
		hangup, err := telnyx.ParseHangupPayload(env)
		if err != nil {
			return err
		}
		// Click-to-call: finalize the CallLog row we created at Dial time.
		if handled, err := a.handleClickToCallHangup(ctx, connection, hangup.ClientState, hangup.HangupCause, hangup.HangupSource); handled {
			return err
		}
		return a.handleTelnyxHangup(ctx, connection, hangup)

	// ---- Recording ----------------------------------------------------

	case telnyx.EventRecordingSaved:
		rec, err := telnyx.ParseRecordingSavedPayload(env)
		if err != nil {
			return err
		}
		// Off the hot path: spawn a tracked goroutine to download.
		a.enqueueTelnyxRecordingDownload(connection.OrganizationID, rec)
		return nil

	// ---- Anything else: log and ignore --------------------------------

	default:
		a.Log.Debug("Telnyx event ignored",
			"event_type", env.Data.EventType,
			"call_control_id", payload.CallControlID)
		return nil
	}
}

// handleTelnyxCallInitiated creates a CallLog row for the new inbound call,
// resolves the contact, finds the IVR flow attached to the called number,
// and starts running it.
func (a *App) handleTelnyxCallInitiated(
	ctx context.Context,
	deps *calling.TelnyxIVRDeps,
	connection *models.TelnyxConnection,
	payload *telnyx.CallEventPayload,
) error {
	if payload.Direction != "incoming" {
		// Outbound calls we initiated ourselves — they have a different
		// flow (Phase 2.3 click-to-call) and the CallLog is created when
		// we POST /v2/calls, not here.
		return nil
	}

	// Look up the iReparo TelnyxNumber by the dialed phone (in E.164
	// without the leading +).
	dialedE164 := calling.PhoneToE164(payload.To)
	var num models.TelnyxNumber
	if err := a.DB.
		Where("phone_number = ? AND connection_id = ?", dialedE164, connection.ID).
		First(&num).Error; err != nil {
		// Number not configured in iReparo — hang up so the caller is
		// not stuck listening to nothing.
		a.Log.Warn("Telnyx call to unconfigured number",
			"called", dialedE164,
			"connection_id", connection.ID)
		_ = deps.Telnyx.Hangup(ctx, payload.CallControlID)
		return fmt.Errorf("number not configured: %s", dialedE164)
	}
	if !num.IsActive {
		_ = deps.Telnyx.Hangup(ctx, payload.CallControlID)
		return fmt.Errorf("number disabled: %s", dialedE164)
	}

	// Resolve the caller into a Contact (or create one).
	callerE164 := calling.PhoneToE164(payload.From)
	contact, _, err := contactutil.GetOrCreateContact(a.DB, connection.OrganizationID, callerE164, "")
	if err != nil || contact == nil {
		_ = deps.Telnyx.Hangup(ctx, payload.CallControlID)
		return fmt.Errorf("get or create contact: %w", err)
	}

	// Pick a default WhatsApp account name to attach to the CallLog so
	// the missed-call WhatsApp fallback can find it later. Falls back
	// gracefully if the org has none.
	waAccountName := a.firstWhatsAppAccountName(connection.OrganizationID)

	now := time.Now().UTC()
	callLog := models.CallLog{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  connection.OrganizationID,
		WhatsAppAccount: waAccountName,
		Channel:         models.CallChannelTelnyxPSTN,
		ContactID:       contact.ID,
		WhatsAppCallID:  payload.CallControlID, // reuse this column for Telnyx control id
		CallerPhone:     callerE164,
		Direction:       models.CallDirectionIncoming,
		Status:          models.CallStatusRinging,
		StartedAt:       &now,
	}
	if num.IVRFlowID != nil {
		callLog.IVRFlowID = num.IVRFlowID
	}
	if err := a.DB.Create(&callLog).Error; err != nil {
		_ = deps.Telnyx.Hangup(ctx, payload.CallControlID)
		return fmt.Errorf("create call log: %w", err)
	}

	// CRM lookup (synchronous, ≤1.5s) — enriches the agent popup with
	// customer info if the caller is in the CRM. No-op if disabled.
	crmLookup, externalID := a.CRMLookupForCall(ctx, contact)

	// Notify the agent panel via WebSocket. The frontend already knows
	// how to render an incoming call notification — we just publish in
	// the same shape as WhatsApp incoming calls, plus optional CRM
	// enrichment fields.
	wsPayload := map[string]any{
		"call_log_id":  callLog.ID.String(),
		"call_id":      payload.CallControlID,
		"caller_phone": callerE164,
		"contact_id":   contact.ID.String(),
		"contact_name": contact.ProfileName,
		"channel":      string(models.CallChannelTelnyxPSTN),
		"started_at":   now.Format(time.RFC3339),
	}
	CRMEnrichBroadcast(wsPayload, crmLookup)
	a.broadcastCallEvent(connection.OrganizationID, websocket.TypeCallIncoming, wsPayload)

	// Emit call.ringing to the CRM (async, retry on failure).
	a.CRMEmitCallEvent(connection.OrganizationID, crm.EventCallRinging, &crm.CallRingingData{
		CallID:        payload.CallControlID,
		Direction:     "incoming",
		CallerPhone:   callerE164,
		CalledPhone:   dialedE164,
		PBXContactID:  contact.ID.String(),
		ExternalCRMID: externalID,
		Channel:       string(models.CallChannelTelnyxPSTN),
	})

	// Load the IVR flow (if any) and start running it.
	if num.IVRFlowID == nil {
		// No flow configured for this number → just hang up.
		_ = deps.Telnyx.Hangup(ctx, payload.CallControlID)
		return nil
	}
	var flow models.IVRFlow
	if err := a.DB.Where("id = ?", *num.IVRFlowID).First(&flow).Error; err != nil {
		_ = deps.Telnyx.Hangup(ctx, payload.CallControlID)
		return fmt.Errorf("load ivr flow: %w", err)
	}

	return calling.StartTelnyxIVR(ctx, deps, payload.CallControlID, &callLog, &flow)
}

// handleTelnyxCallAnswered marks the CallLog as answered when the bridge
// (or the agent leg) picks up.
func (a *App) handleTelnyxCallAnswered(
	ctx context.Context,
	deps *calling.TelnyxIVRDeps,
	connection *models.TelnyxConnection,
	payload *telnyx.CallEventPayload,
) error {
	now := time.Now().UTC()
	updates := map[string]any{
		"status":      models.CallStatusAnswered,
		"answered_at": now,
	}
	if err := a.DB.Model(&models.CallLog{}).
		Where("whatsapp_call_id = ? AND organization_id = ?", payload.CallControlID, connection.OrganizationID).
		Updates(updates).Error; err != nil {
		return fmt.Errorf("update call log answered: %w", err)
	}

	a.broadcastCallEvent(connection.OrganizationID, websocket.TypeCallAnswered, map[string]any{
		"call_id":     payload.CallControlID,
		"answered_at": now.Format(time.RFC3339),
		"channel":     string(models.CallChannelTelnyxPSTN),
	})

	// Look up the contact again to get the cached external_crm_id for the
	// CRM event payload.
	var cl models.CallLog
	var contact models.Contact
	if err := a.DB.Where("whatsapp_call_id = ?", payload.CallControlID).First(&cl).Error; err == nil {
		_ = a.DB.Where("id = ?", cl.ContactID).First(&contact).Error
	}
	a.CRMEmitCallEvent(connection.OrganizationID, crm.EventCallAnswered, &crm.CallAnsweredData{
		CallID:        payload.CallControlID,
		AnsweredAt:    now,
		Channel:       string(models.CallChannelTelnyxPSTN),
		ExternalCRMID: externalCRMIDOrNil(&contact),
	})
	return nil
}

// markTelnyxBridged updates the CallLog when two legs successfully bridge
// (a transfer landed). For now we just record the event time; a future
// commit will create a proper CallTransfer row.
func (a *App) markTelnyxBridged(orgID uuid.UUID, payload *telnyx.CallEventPayload) {
	a.broadcastCallEvent(orgID, websocket.TypeCallAnswered, map[string]any{
		"call_id": payload.CallControlID,
		"channel": string(models.CallChannelTelnyxPSTN),
		"bridged": true,
	})
}

// handleTelnyxHangup is the terminal step for any Telnyx call. It computes
// the final duration, updates the CallLog, broadcasts a call_ended event,
// and triggers the missed-call WhatsApp fallback if applicable.
func (a *App) handleTelnyxHangup(
	ctx context.Context,
	connection *models.TelnyxConnection,
	hangup *telnyx.CallHangupPayload,
) error {
	now := time.Now().UTC()

	// Reload the CallLog by Telnyx call control id (we stored it in
	// whatsapp_call_id when the call was created).
	var callLog models.CallLog
	if err := a.DB.
		Where("whatsapp_call_id = ? AND organization_id = ?", hangup.CallControlID, connection.OrganizationID).
		First(&callLog).Error; err != nil {
		// No matching CallLog. Nothing to update; the call probably
		// originated outside of iReparo somehow.
		return fmt.Errorf("call log not found for hangup: %w", err)
	}

	// Determine final status.
	finalStatus := models.CallStatusCompleted
	durationSec := 0
	if callLog.AnsweredAt != nil {
		durationSec = int(now.Sub(*callLog.AnsweredAt).Seconds())
	} else {
		// Never answered → missed.
		finalStatus = models.CallStatusMissed
	}

	updates := map[string]any{
		"status":   finalStatus,
		"ended_at": now,
		"duration": durationSec,
	}
	if callLog.DisconnectedBy == "" {
		who := models.DisconnectedByClient
		if strings.EqualFold(hangup.HangupSource, "callee") {
			who = models.DisconnectedByAgent
		} else if strings.EqualFold(hangup.HangupSource, "system") {
			who = models.DisconnectedBySystem
		}
		updates["disconnected_by"] = who
	}
	if hangup.HangupCause != "" {
		updates["error_message"] = "telnyx_hangup_cause:" + hangup.HangupCause
	}
	if err := a.DB.Model(&callLog).Updates(updates).Error; err != nil {
		return fmt.Errorf("update call log hangup: %w", err)
	}

	// Notify the agent panel.
	disconnectedByStr := ""
	if v, ok := updates["disconnected_by"].(models.DisconnectedBy); ok {
		disconnectedByStr = string(v)
	}
	a.broadcastCallEvent(connection.OrganizationID, websocket.TypeCallEnded, map[string]any{
		"call_id":         hangup.CallControlID,
		"call_log_id":     callLog.ID.String(),
		"status":          string(finalStatus),
		"duration":        durationSec,
		"ended_at":        now.Format(time.RFC3339),
		"disconnected_by": disconnectedByStr,
		"channel":         string(models.CallChannelTelnyxPSTN),
	})

	// Look up the contact for the CRM event payload.
	var contact models.Contact
	_ = a.DB.Where("id = ?", callLog.ContactID).First(&contact).Error
	extID := externalCRMIDOrNil(&contact)

	// Emit call.ended (or call.missed) to the CRM.
	if finalStatus == models.CallStatusMissed {
		a.CRMEmitCallEvent(connection.OrganizationID, crm.EventCallMissed, &crm.CallMissedData{
			CallID:               hangup.CallControlID,
			CallerPhone:          callLog.CallerPhone,
			CalledPhone:          "", // Telnyx hangup payload does not always carry it
			PBXContactID:         callLog.ContactID.String(),
			ExternalCRMID:        extID,
			Reason:               "no_agent_available",
			Channel:              string(models.CallChannelTelnyxPSTN),
			WhatsappFallbackSent: true,
		})
	} else {
		a.CRMEmitCallEvent(connection.OrganizationID, crm.EventCallEnded, &crm.CallEndedData{
			CallID:          hangup.CallControlID,
			Direction:       string(callLog.Direction),
			CallerPhone:     callLog.CallerPhone,
			CalledPhone:     "",
			PBXContactID:    callLog.ContactID.String(),
			ExternalCRMID:   extID,
			Status:          string(finalStatus),
			DurationSeconds: durationSec,
			StartedAt:       deref(callLog.StartedAt),
			AnsweredAt:      callLog.AnsweredAt,
			EndedAt:         now,
			DisconnectedBy:  disconnectedByStr,
			Channel:         string(models.CallChannelTelnyxPSTN),
		})
	}

	// Hook the existing missed-call WhatsApp fallback. The function is
	// channel-agnostic — it works on any CallLog that ended in
	// CallStatusMissed.
	if finalStatus == models.CallStatusMissed {
		callLog.Status = finalStatus
		callLog.EndedAt = &now
		a.TriggerMissedCallWhatsApp(&callLog)
	}

	return nil
}

// firstWhatsAppAccountName returns the name of any WhatsApp account in the
// organization, preferring the default outgoing one. Used as a fallback so
// CallLog.WhatsAppAccount is never empty (the missed-call fallback may
// need it later to send the WhatsApp template).
//
// Returns "" if the org has no WhatsApp accounts at all (e.g., a Telnyx-
// only deployment).
func (a *App) firstWhatsAppAccountName(orgID uuid.UUID) string {
	var account models.WhatsAppAccount
	if err := a.DB.
		Where("organization_id = ?", orgID).
		Order("is_default_outgoing DESC, created_at ASC").
		First(&account).Error; err != nil {
		return ""
	}
	return account.Name
}
