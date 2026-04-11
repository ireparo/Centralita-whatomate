package handlers

import (
	"encoding/json"

	"github.com/shridarpatil/whatomate/internal/integrations/telnyx"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// TelnyxWebhookHandler is the public endpoint Telnyx posts call events to.
//
// In Phase 2.1 (this commit) the handler is a STUB: it parses the envelope,
// looks up the matching TelnyxConnection by Call Control App ID, verifies
// the Ed25519 signature, and logs every event. It does NOT yet drive the
// IVR or create CallLog rows — that arrives in Phase 2.2.
//
// Wiring it up early lets us:
//   - Have Telnyx accept the webhook URL when configuring the Call Control
//     App in the Telnyx panel (Telnyx pings it once on save).
//   - Capture real event payloads from the first incoming test call so the
//     Phase 2.2 logic can be developed against real data instead of mocks.
//
// Route: POST /api/webhook/telnyx (public, rate-limited)
func (a *App) TelnyxWebhookHandler(r *fastglue.Request) error {
	body := r.RequestCtx.PostBody()

	// 1. Parse the envelope first so we know which connection_id sent it.
	var env telnyx.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		a.Log.Warn("Telnyx webhook: invalid JSON", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid JSON", nil, "")
	}

	// 2. Extract the connection ID from the payload (every call event carries
	//    it). Telnyx puts it in the payload object.
	payload, err := telnyx.ParseCallPayload(&env)
	if err != nil {
		// Not a call event we know about — log and 200 so Telnyx does not
		// retry forever.
		a.Log.Debug("Telnyx webhook: ignoring non-call event",
			"event_type", env.Data.EventType,
			"id", env.Data.ID)
		return r.SendEnvelope(map[string]string{"status": "ignored"})
	}

	// 3. Look up the iReparo connection that owns this Call Control App.
	var connection models.TelnyxConnection
	if err := a.DB.
		Where("call_control_app_id = ?", payload.ConnectionID).
		First(&connection).Error; err != nil {
		a.Log.Warn("Telnyx webhook: no connection registered for Call Control App",
			"call_control_app_id", payload.ConnectionID,
			"event_type", env.Data.EventType)
		// Return 200 to stop Telnyx from spamming retries on a connection
		// that does not exist on our side.
		return r.SendEnvelope(map[string]string{"status": "unknown_connection"})
	}
	connection.DecryptSecrets(a.Config.App.EncryptionKey)

	// 4. Verify Ed25519 signature using the public key of this connection.
	signature := string(r.RequestCtx.Request.Header.Peek(telnyx.HeaderSignature))
	timestamp := string(r.RequestCtx.Request.Header.Peek(telnyx.HeaderTimestamp))

	if connection.PublicKey == "" {
		// First-time setup: connection saved without a public key yet.
		// Log a warning so the operator notices, but accept the request.
		a.Log.Warn("Telnyx webhook: signature NOT verified — public key not configured yet for connection",
			"connection_id", connection.ID,
			"organization_id", connection.OrganizationID)
	} else if err := telnyx.VerifyWebhook(connection.PublicKey, signature, timestamp, body); err != nil {
		a.Log.Warn("Telnyx webhook: signature verification failed — rejecting request",
			"connection_id", connection.ID,
			"error", err)
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Invalid signature", nil, "")
	}

	// 5. Log the event. Phase 2.2 will replace this with the dispatch logic
	//    that creates CallLog rows, drives the IVR engine and broadcasts
	//    WebSocket events to the agent panel.
	a.Log.Info("Telnyx webhook event",
		"event_type", env.Data.EventType,
		"event_id", env.Data.ID,
		"call_control_id", payload.CallControlID,
		"call_session_id", payload.CallSessionID,
		"from", payload.From,
		"to", payload.To,
		"direction", payload.Direction,
		"state", payload.State,
		"organization_id", connection.OrganizationID)

	return r.SendEnvelope(map[string]string{"status": "accepted"})
}
