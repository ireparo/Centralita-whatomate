package handlers

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/integrations/crm"
	"github.com/shridarpatil/whatomate/internal/models"
)

// crm_hooks.go contains the helper functions that bridge the call/message
// pipelines with the external CRM (Phase 3). All helpers are no-ops when
// CRM integration is disabled, so the call sites can call them
// unconditionally.

// CRMLookupForCall queries the CRM for the caller phone, with a tight
// timeout. On success it caches the customer's external_crm_id on the
// local Contact row so subsequent calls can skip the network round-trip.
//
// Returns the LookupResponse if found (or nil), and the contact's updated
// external_crm_id (or nil if still unknown). Errors are swallowed and
// logged at debug level — the caller continues with "unknown" UX on any
// CRM hiccup.
func (a *App) CRMLookupForCall(ctx context.Context, contact *models.Contact) (*crm.LookupResponse, *int64) {
	if a.CRM == nil || !a.CRM.Enabled() || contact == nil || contact.PhoneNumber == "" {
		return nil, externalCRMIDOrNil(contact)
	}

	// Bound by the configured lookup timeout — never block the call
	// pipeline waiting for the CRM. The agent panel must show
	// SOMETHING within ~2 seconds even if the CRM is down.
	lookupCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()

	resp, err := a.CRM.Lookup(lookupCtx, contact.PhoneNumber)
	if err != nil {
		a.Log.Debug("CRM lookup failed (continuing with unknown caller)",
			"phone", contact.PhoneNumber,
			"error", err)
		return nil, externalCRMIDOrNil(contact)
	}
	if !resp.Found || resp.Customer == nil {
		return resp, nil
	}

	// First time we see this customer — cache its CRM id locally so we
	// do not need to look up again on the next call.
	if contact.ExternalCRMID == nil || *contact.ExternalCRMID != resp.Customer.ID {
		newID := resp.Customer.ID
		contact.ExternalCRMID = &newID
		_ = a.DB.Model(contact).Update("external_crm_id", newID).Error
	}
	return resp, contact.ExternalCRMID
}

// CRMEnrichBroadcast adds the relevant CRM customer fields to a WebSocket
// broadcast payload so the agent panel can render the screen-pop with rich
// data instead of "Unknown caller".
//
// Mutates the supplied map in place.
func CRMEnrichBroadcast(payload map[string]any, lookup *crm.LookupResponse) {
	if payload == nil || lookup == nil || !lookup.Found || lookup.Customer == nil {
		return
	}
	c := lookup.Customer
	payload["crm_customer_id"] = c.ID
	payload["crm_customer_name"] = c.Name
	payload["crm_profile_url"] = c.ProfileURL
	payload["crm_active_tickets_count"] = c.ActiveTicketsCount
	payload["crm_total_spent_eur"] = c.TotalSpentEUR
	payload["crm_vip"] = c.VIP
	if c.LastTicket != nil {
		payload["crm_last_ticket"] = map[string]any{
			"id":             c.LastTicket.ID,
			"tracking_token": c.LastTicket.TrackingToken,
			"status":         c.LastTicket.Status,
			"device":         c.LastTicket.Device,
			"opened_at":      c.LastTicket.OpenedAt,
			"url":            c.LastTicket.URL,
		}
	}
}

// CRMEmitCallEvent builds a CRM event envelope for any of the call.* events
// and sends it asynchronously, falling back to the persistent retry queue
// on failure. No-op if CRM integration is disabled.
//
// Always returns immediately — the actual HTTP send happens in a tracked
// goroutine so the call pipeline is never blocked by CRM latency.
func (a *App) CRMEmitCallEvent(orgID uuid.UUID, eventType string, data any) {
	if a.CRM == nil || !a.CRM.Enabled() {
		return
	}
	env, err := a.CRM.BuildEvent(eventType, data)
	if err != nil {
		a.Log.Debug("CRM build event failed", "event_type", eventType, "error", err)
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := a.CRM.Send(ctx, env); err == nil {
			return
		} else {
			a.Log.Debug("CRM event Send failed, enqueuing for retry",
				"event_type", eventType, "error", err)
		}

		// Persist for retry by the queue worker.
		if err := crm.EnqueueEvent(a.DB, orgID, env); err != nil {
			a.Log.Warn("CRM event enqueue failed (event lost)",
				"event_type", eventType, "error", err)
		}
	}()
}

// externalCRMIDOrNil is a small helper to safely return the contact's
// external CRM id without nil-deref panics.
func externalCRMIDOrNil(c *models.Contact) *int64 {
	if c == nil {
		return nil
	}
	return c.ExternalCRMID
}

// deref returns the value pointed to by t, or the zero value of time.Time
// if t is nil. Used to avoid lots of inline nil checks when building CRM
// event payloads.
func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// CRMEmitMessageEvent builds a CRM event envelope for message.inbound /
// message.outbound and sends it asynchronously with the same
// fire-and-forget + persistent-retry semantics as CRMEmitCallEvent.
//
// The helper is a no-op if the CRM integration is disabled or if data is
// nil, so callers in the message pipeline can call it unconditionally.
func (a *App) CRMEmitMessageEvent(orgID uuid.UUID, eventType string, data any) {
	if a.CRM == nil || !a.CRM.Enabled() || data == nil {
		return
	}
	env, err := a.CRM.BuildEvent(eventType, data)
	if err != nil {
		a.Log.Debug("CRM build message event failed", "event_type", eventType, "error", err)
		return
	}

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := a.CRM.Send(ctx, env); err == nil {
			return
		} else {
			a.Log.Debug("CRM message event Send failed, enqueuing for retry",
				"event_type", eventType, "error", err)
		}
		if err := crm.EnqueueEvent(a.DB, orgID, env); err != nil {
			a.Log.Warn("CRM message event enqueue failed (event lost)",
				"event_type", eventType, "error", err)
		}
	}()
}

// buildInboundMessageData is a small helper that produces the CRM payload
// for an incoming WhatsApp message. Keeping it in one place makes it easy
// to keep fields in sync with the Go struct in
// internal/integrations/crm/events.go.
func buildInboundMessageData(
	msg *models.Message,
	contact *models.Contact,
	account *models.WhatsAppAccount,
) *crm.MessageInboundData {
	if msg == nil || contact == nil || account == nil {
		return nil
	}
	return &crm.MessageInboundData{
		MessageID:       msg.WhatsAppMessageID,
		FromPhone:       crm.NormalizePhone(contact.PhoneNumber),
		PBXContactID:    contact.ID.String(),
		ExternalCRMID:   externalCRMIDOrNil(contact),
		Type:            string(msg.MessageType),
		Content:         msg.Content,
		MediaURL:        msg.MediaURL,
		WhatsAppAccount: account.Name,
	}
}

// buildOutboundMessageData is the counterpart of buildInboundMessageData
// for outgoing messages. It infers the sender type from the combination of
// SentByUserID (agent if set) + message type (template / interactive / etc.)
// so the CRM can branch cleanly on who initiated the send.
func (a *App) buildOutboundMessageData(
	msg *models.Message,
	contact *models.Contact,
	account *models.WhatsAppAccount,
) *crm.MessageOutboundData {
	if msg == nil || contact == nil || account == nil {
		return nil
	}
	return &crm.MessageOutboundData{
		MessageID:       msg.WhatsAppMessageID,
		ToPhone:         crm.NormalizePhone(contact.PhoneNumber),
		PBXContactID:    contact.ID.String(),
		ExternalCRMID:   externalCRMIDOrNil(contact),
		Type:            string(msg.MessageType),
		Content:         msg.Content,
		SentBy:          a.resolveMessageSender(msg),
		WhatsAppAccount: account.Name,
	}
}

// resolveMessageSender classifies who triggered an outbound message. The
// iReparo message model has no explicit sender_type, so we infer from
// SentByUserID (agent if non-nil) and MessageType as a fallback signal.
//
// Categories:
//
//   - agent                — SentByUserID is set (UI or API-on-behalf-of-agent)
//   - template             — MessageType=template with no agent
//   - missed_call_fallback — MessageType=template and the template is the
//                            configured missed-call-fallback template. The
//                            template name match is a best-effort hint
//                            (see WhatsAppAccount.MissedCallWhatsAppTemplate).
//   - chatbot              — MessageType interactive/flow/button with no agent
//   - campaign             — default fallback when none of the above fire
//
// The classification is advisory; the CRM can always look at the message
// type + agent fields in the payload if it needs more detail.
func (a *App) resolveMessageSender(msg *models.Message) *crm.MessageSenderInfo {
	if msg == nil {
		return nil
	}
	info := &crm.MessageSenderInfo{}

	if msg.SentByUserID != nil {
		info.Type = "agent"
		info.AgentID = msg.SentByUserID.String()
		// Best-effort: fetch the user's full name. Errors are ignored so we
		// still send the event — the agent name is informational.
		var user models.User
		if err := a.DB.
			Select("id, full_name").
			Where("id = ?", *msg.SentByUserID).
			First(&user).Error; err == nil {
			info.AgentName = user.FullName
		}
		return info
	}

	switch msg.MessageType {
	case models.MessageTypeTemplate:
		info.Type = "template"
	case models.MessageTypeInteractive, models.MessageTypeFlow:
		info.Type = "chatbot"
	default:
		info.Type = "campaign"
	}
	return info
}
