package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/integrations/telnyx"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// telnyx_click_to_call.go implements the callback-style click-to-call flow:
//
//   1. Agent clicks "Call" on a contact. Frontend POSTs
//      /api/calls/telnyx/click-to-call with {contact_id}.
//   2. Backend validates the agent has a phone_number on their User row.
//   3. Backend picks a FROM number: the first active TelnyxNumber for the
//      org's single connection. (Future: per-user default number.)
//   4. Backend calls telnyx.Client.Dial(to: agentPhone, from: ourNumber,
//      client_state: <agent_leg marker>). A CallLog row is created with
//      direction=outgoing, channel=telnyx_pstn, status=ringing.
//   5. Agent's phone rings. When they pick up, Telnyx fires call.answered
//      with the same client_state. The webhook handler recognizes the
//      agent_leg marker and issues a telnyx.Transfer to the customer's
//      phone, merging the two legs.
//   6. Call ends normally; call.hangup finalizes the CallLog row.
//
// This uses Telnyx's Transfer action (not a conference) because the agent's
// leg is already active — Transfer tells Telnyx to dial the target and
// bridge as a second leg. It is the simplest pattern and uses no per-call
// state beyond what Telnyx's client_state field already carries.

// ClickToCallRequest is the POST body for POST /api/calls/telnyx/click-to-call.
type ClickToCallRequest struct {
	ContactID string `json:"contact_id"`
	// FromNumber optionally forces the FROM number (E.164 no +). When empty
	// the handler picks the first active TelnyxNumber on the org's
	// connection.
	FromNumber string `json:"from_number,omitempty"`
}

// ClickToCallState is the marker payload encoded in Telnyx's client_state
// for the agent leg of a click-to-call flow. It lets the webhook handler
// recognize the agent-answered event and trigger the Transfer to the
// customer instead of running the IVR dispatcher.
//
// The `kind` field lets us extend this envelope with other callback-style
// flows later (e.g. "campaign", "ringback_transfer") without rewriting the
// webhook handler.
type ClickToCallState struct {
	Kind         string `json:"k"`     // always "c2c_agent" for this flow
	CallLogID    string `json:"clog"`  // CallLog UUID to update on lifecycle events
	TargetPhone  string `json:"tgt"`   // E.164 no "+" — the customer we're bridging to
	FromPhone    string `json:"from"`  // E.164 no "+" — the caller_id Telnyx uses for the transfer
	OrgID        string `json:"org"`
	InitiatedBy  string `json:"by"`    // User UUID who clicked the button
}

// EncodeClickToCallState serializes the marker to the same base64(JSON)
// layout the IVR dispatcher uses, so the two can coexist in client_state
// without collisions — we discriminate by the "k" field on decode.
func EncodeClickToCallState(s *ClickToCallState) (string, error) {
	s.Kind = "c2c_agent"
	buf, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode c2c state: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// DecodeClickToCallState tries to decode a client_state value as a
// ClickToCallState. Returns (nil, nil) if the value is valid base64 JSON
// but doesn't carry our marker — callers should then fall through to the
// IVR state decoder.
func DecodeClickToCallState(encoded string) (*ClickToCallState, error) {
	if encoded == "" {
		return nil, errors.New("empty client_state")
	}
	buf, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode c2c state: %w", err)
	}
	var s ClickToCallState
	if err := json.Unmarshal(buf, &s); err != nil {
		return nil, fmt.Errorf("unmarshal c2c state: %w", err)
	}
	if s.Kind != "c2c_agent" {
		return nil, nil // not a click-to-call state
	}
	return &s, nil
}

// InitiateClickToCall handles POST /api/calls/telnyx/click-to-call.
func (a *App) InitiateClickToCall(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceOutgoingCalls, models.ActionExecute, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to place outbound calls", nil, "")
	}

	var req ClickToCallRequest
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	contactID, err := uuid.Parse(req.ContactID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact_id", nil, "")
	}

	// Load the agent and validate their phone is set.
	var agent models.User
	if err := a.DB.Where("id = ?", userID).First(&agent).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load user", nil, "")
	}
	agentPhone := calling.PhoneToE164(agent.PhoneNumber)
	if agentPhone == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Your user profile has no phone number configured. Set one under Settings → Profile.",
			nil, "missing_agent_phone")
	}

	// Load the contact and normalize their phone.
	var contact models.Contact
	if err := a.DB.Where("id = ? AND organization_id = ?", contactID, orgID).First(&contact).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
	}
	targetPhone := calling.PhoneToE164(contact.PhoneNumber)
	if targetPhone == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Contact has no valid phone number", nil, "")
	}

	// Load the org's Telnyx connection (one per org).
	var conn models.TelnyxConnection
	if err := a.DB.Where("organization_id = ?", orgID).First(&conn).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusFailedDependency,
				"No Telnyx connection configured. Set one up under Settings → PSTN Telephony.",
				nil, "telnyx_not_configured")
		}
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load Telnyx connection", nil, "")
	}
	conn.DecryptSecrets(a.Config.App.EncryptionKey)

	// Pick the FROM number: explicit request or first active DDI.
	fromPhone := calling.PhoneToE164(req.FromNumber)
	if fromPhone == "" {
		var number models.TelnyxNumber
		if err := a.DB.Where("connection_id = ? AND is_active = ?", conn.ID, true).
			Order("created_at ASC").First(&number).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusFailedDependency,
				"No active Telnyx number available as caller ID. Add one under Settings → PSTN Telephony.",
				nil, "no_outbound_number")
		}
		fromPhone = number.PhoneNumber
	}

	// Create the CallLog row up front so the webhook handler can look it up
	// on call.answered / call.hangup without guessing.
	now := time.Now().UTC()
	callLog := models.CallLog{
		BaseModel:       models.BaseModel{ID: uuid.New()},
		OrganizationID:  orgID,
		Channel:         models.CallChannelTelnyxPSTN,
		Direction:       models.CallDirectionOutgoing,
		Status:          models.CallStatusRinging,
		CallerPhone:     fromPhone,
		ContactID:       contact.ID,
		AgentID:         &userID,
		StartedAt:       &now,
		WhatsAppAccount: "", // not a WhatsApp call
	}
	if err := a.DB.Create(&callLog).Error; err != nil {
		a.Log.Error("click-to-call: create CallLog failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create call log", nil, "")
	}

	// Encode the marker so call.answered on the agent leg triggers Transfer.
	state := &ClickToCallState{
		CallLogID:   callLog.ID.String(),
		TargetPhone: targetPhone,
		FromPhone:   fromPhone,
		OrgID:       orgID.String(),
		InitiatedBy: userID.String(),
	}
	encoded, err := EncodeClickToCallState(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to encode call state", nil, "")
	}

	// Dial the AGENT first. Telnyx accepts E.164 with or without "+", but
	// the official format is with + — prepend it for safety.
	tnx := telnyx.NewClient(conn.APIKey, a.HTTPClient)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial, err := tnx.Dial(ctx, &telnyx.DialRequest{
		To:           "+" + agentPhone,
		From:         "+" + fromPhone,
		ConnectionID: conn.CallControlAppID,
		ClientState:  encoded,
		// Do not enable recording on the agent leg — the recording starts
		// once the legs are bridged (see handleTelnyxCallAnswered path for
		// click-to-call below). This keeps hold/transfer audio out.
	})
	if err != nil {
		// Mark the CallLog as failed so it shows up in reporting. Error
		// bubbled to the caller so the UI can show "Failed to dial".
		_ = a.DB.Model(&callLog).Updates(map[string]any{
			"status":        models.CallStatusFailed,
			"error_message": err.Error(),
			"ended_at":      time.Now().UTC(),
		}).Error
		a.Log.Warn("click-to-call: Telnyx Dial failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Telnyx dial failed: "+err.Error(), nil, "")
	}

	// Persist the Telnyx CallControlID so inbound webhooks can correlate.
	_ = a.DB.Model(&callLog).Update("whatsapp_call_id", dial.CallControlID).Error

	return r.SendEnvelope(map[string]any{
		"call_log_id":    callLog.ID,
		"call_control_id": dial.CallControlID,
		"to":             agentPhone,
		"from":           fromPhone,
		"status":         "ringing_agent",
	})
}

// handleClickToCallAgentAnswered is invoked from the Telnyx webhook dispatcher
// when a call.answered event arrives AND the client_state decodes as a
// click-to-call marker. It issues a Transfer to the customer's phone so the
// two legs get bridged.
//
// Returns true when the event was handled by this flow (caller should not
// run the IVR dispatcher), false otherwise.
func (a *App) handleClickToCallAgentAnswered(
	ctx context.Context,
	conn *models.TelnyxConnection,
	callControlID string,
	clientState string,
) (bool, error) {
	state, err := DecodeClickToCallState(clientState)
	if err != nil || state == nil {
		return false, nil // not click-to-call, let IVR handler take it
	}

	tnx := telnyx.NewClient(conn.APIKey, a.HTTPClient)

	// Before transferring, mark the CallLog as "answered" by the agent so
	// the timeline in the UI shows when the bridge started.
	now := time.Now().UTC()
	if state.CallLogID != "" {
		if callLogID, err := uuid.Parse(state.CallLogID); err == nil {
			_ = a.DB.Model(&models.CallLog{}).
				Where("id = ? AND organization_id = ?", callLogID, conn.OrganizationID).
				Updates(map[string]any{
					"status":      models.CallStatusAccepted, // "accepted" = customer-leg-not-yet-answered
					"answered_at": now,
				}).Error
		}
	}

	// Transfer the agent's active call leg to the customer. Reuse the same
	// client_state so the customer-leg webhook events (bridged, hangup)
	// can still find the CallLog.
	transferErr := tnx.Transfer(ctx, callControlID, &telnyx.TransferRequest{
		To:          "+" + state.TargetPhone,
		From:        "+" + state.FromPhone,
		ClientState: clientState,
	})
	if transferErr != nil {
		a.Log.Warn("click-to-call: Transfer failed", "error", transferErr, "call_control_id", callControlID)
		// Let Telnyx hang up on its own — we don't have a clean way to
		// keep the agent leg alive if the transfer fails.
		return true, transferErr
	}
	return true, nil
}

// handleClickToCallHangup is invoked from the Telnyx webhook dispatcher on
// call.hangup events that carry a click-to-call marker. Finalizes the
// CallLog row with duration and disconnected_by.
//
// Returns true when the event was owned by this flow.
func (a *App) handleClickToCallHangup(
	_ context.Context,
	conn *models.TelnyxConnection,
	clientState string,
	hangupCause string,
	hangupSource string,
) (bool, error) {
	state, err := DecodeClickToCallState(clientState)
	if err != nil || state == nil {
		return false, nil
	}
	if state.CallLogID == "" {
		return true, nil
	}
	callLogID, err := uuid.Parse(state.CallLogID)
	if err != nil {
		return true, nil
	}

	var callLog models.CallLog
	if err := a.DB.Where("id = ? AND organization_id = ?", callLogID, conn.OrganizationID).
		First(&callLog).Error; err != nil {
		return true, err
	}

	now := time.Now().UTC()
	updates := map[string]any{
		"ended_at":        now,
		"disconnected_by": classifyHangupSource(hangupSource),
	}
	// Duration from StartedAt is a reasonable total (agent ringing + bridge).
	if callLog.StartedAt != nil {
		updates["duration"] = int(now.Sub(*callLog.StartedAt).Seconds())
	}
	// Pick a final status consistent with existing inbound Telnyx flow.
	if callLog.Status == models.CallStatusRinging {
		// Agent never picked up.
		updates["status"] = models.CallStatusMissed
	} else {
		updates["status"] = models.CallStatusCompleted
	}
	if hangupCause != "" {
		updates["error_message"] = hangupCause
	}
	if err := a.DB.Model(&callLog).Updates(updates).Error; err != nil {
		return true, err
	}
	return true, nil
}

// classifyHangupSource maps Telnyx's hangup_source field to our own enum.
// Telnyx sends values like "caller", "callee", "client", "call_control_app".
func classifyHangupSource(src string) models.DisconnectedBy {
	switch strings.ToLower(src) {
	case "caller":
		return models.DisconnectedByAgent // the agent leg we initiated is our "caller"
	case "callee":
		return models.DisconnectedByClient
	case "call_control_app", "client":
		return models.DisconnectedBySystem
	}
	return models.DisconnectedBySystem
}
