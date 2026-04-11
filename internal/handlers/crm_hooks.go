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
