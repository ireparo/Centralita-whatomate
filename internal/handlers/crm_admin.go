package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/integrations/crm"
	"github.com/shridarpatil/whatomate/internal/integrations/telnyx"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// crm_admin.go exposes admin endpoints for operating on the CRM delivery
// queue — specifically the dead-letter queue. All endpoints are:
//
//   - Org-scoped (rows returned / modified belong to the caller's org).
//   - Permission-gated on settings.general (admin-ish operation).
//
// Routes:
//
//	GET    /api/admin/crm-queue          — list queue rows (filters + pagination)
//	POST   /api/admin/crm-queue/{id}/replay — requeue a dead-lettered event
//	DELETE /api/admin/crm-queue/{id}     — drop an event permanently

// CRMQueueRow is the JSON view of a models.CRMEventQueue row, trimmed and
// reshaped so the admin UI can render it directly. The raw signed payload
// is included (truncated) so operators can diff what was actually sent.
type CRMQueueRow struct {
	ID            uuid.UUID  `json:"id"`
	EventType     string     `json:"event_type"`
	Endpoint      string     `json:"endpoint"`
	Status        string     `json:"status"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	PayloadPreview string    `json:"payload_preview"`
}

// CRMQueueListResponse is the envelope returned by GET /api/admin/crm-queue.
type CRMQueueListResponse struct {
	Rows       []CRMQueueRow `json:"rows"`
	Total      int64         `json:"total"`
	Pending    int64         `json:"pending"`
	DeadLetter int64         `json:"dead_letter"`
	Delivered  int64         `json:"delivered"`
}

// ListCRMEventQueue returns the rows of crm_event_queue for the current
// organization. Supports filter=status and basic limit/offset pagination.
//
// Typical call: GET /api/admin/crm-queue?status=dead_letter&limit=50
func (a *App) ListCRMEventQueue(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to view the CRM queue", nil, "")
	}

	q := r.RequestCtx.QueryArgs()
	status := string(q.Peek("status"))
	limit, _ := strconv.Atoi(string(q.Peek("limit")))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	offset, _ := strconv.Atoi(string(q.Peek("offset")))
	if offset < 0 {
		offset = 0
	}

	db := a.DB.Model(&models.CRMEventQueue{}).Where("organization_id = ?", orgID)
	if status != "" {
		db = db.Where("status = ?", status)
	}

	var rows []models.CRMEventQueue
	if err := db.Order("created_at DESC").Limit(limit).Offset(offset).Find(&rows).Error; err != nil {
		a.Log.Error("CRM queue list failed", "error", err, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list queue", nil, "")
	}

	// Per-status counters — cheap enough to compute on every list call, and
	// the UI uses them to render the status filter dropdown with counts.
	counts := map[string]int64{}
	type row struct {
		Status string
		N      int64
	}
	var countRows []row
	_ = a.DB.Model(&models.CRMEventQueue{}).
		Select("status, COUNT(*) AS n").
		Where("organization_id = ?", orgID).
		Group("status").
		Scan(&countRows).Error
	for _, cr := range countRows {
		counts[cr.Status] = cr.N
	}

	out := CRMQueueListResponse{
		Rows:       make([]CRMQueueRow, 0, len(rows)),
		Pending:    counts["pending"],
		DeadLetter: counts["dead_letter"],
		Delivered:  counts["delivered"],
	}
	out.Total = out.Pending + out.DeadLetter + out.Delivered

	for _, row := range rows {
		preview := row.Payload
		if len(preview) > 512 {
			preview = preview[:512] + "..."
		}
		out.Rows = append(out.Rows, CRMQueueRow{
			ID:             row.ID,
			EventType:      row.EventType,
			Endpoint:       row.Endpoint,
			Status:         row.Status,
			AttemptCount:   row.AttemptCount,
			NextAttemptAt:  row.NextAttemptAt,
			LastError:      row.LastError,
			DeliveredAt:    row.DeliveredAt,
			CreatedAt:      row.CreatedAt,
			PayloadPreview: preview,
		})
	}
	return r.SendEnvelope(out)
}

// ReplayCRMEventQueue marks a previously dead-lettered or failed event as
// pending and either sends it immediately (best-effort) or lets the
// background worker pick it up on the next tick.
//
// The signature on the stored row is reused as-is: the CRM-side receiver
// tolerates timestamp skew for known (call_id, event) pairs.
func (a *App) ReplayCRMEventQueue(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to replay CRM events", nil, "")
	}

	idParam := r.RequestCtx.UserValue("id")
	idStr, ok := idParam.(string)
	if !ok || idStr == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Missing id", nil, "")
	}
	rowID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	var row models.CRMEventQueue
	if err := a.DB.Where("id = ? AND organization_id = ?", rowID, orgID).First(&row).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Event not found", nil, "")
	}

	// Reset state to pending so the worker picks it up on the next tick.
	// We also clear next_attempt_at so the very next tick (within 15s)
	// attempts delivery, rather than waiting for the previous backoff.
	updates := map[string]any{
		"status":          "pending",
		"attempt_count":   0,
		"next_attempt_at": nil,
		"last_error":      "",
		"delivered_at":    nil,
	}
	if err := a.DB.Model(&row).Updates(updates).Error; err != nil {
		a.Log.Error("CRM queue replay: reset row failed", "error", err, "id", rowID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to replay", nil, "")
	}

	// Best-effort immediate send so the admin sees the result without
	// waiting for the next worker tick. Failures just leave the row
	// pending for the worker to retry.
	if a.CRM != nil && a.CRM.Enabled() {
		env := &crm.EventEnvelope{
			EventType: row.EventType,
			Body:      []byte(row.Payload),
			Signature: row.Signature,
			Timestamp: row.Timestamp,
			URL:       row.Endpoint,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if sendErr := a.CRM.Send(ctx, env); sendErr == nil {
			now := time.Now().UTC()
			_ = a.DB.Model(&row).Updates(map[string]any{
				"status":        "delivered",
				"delivered_at":  now,
				"attempt_count": 1,
			}).Error
			return r.SendEnvelope(map[string]any{"status": "delivered"})
		} else {
			a.Log.Debug("CRM queue replay: immediate send failed, worker will retry",
				"id", rowID, "error", sendErr)
		}
	}
	return r.SendEnvelope(map[string]any{"status": "pending"})
}

// DeleteCRMEventQueue permanently removes a queue row. Typically used for
// stale dead-letter entries that are no longer relevant (e.g. a call that
// was made during a CRM outage and is already reconciled manually).
func (a *App) DeleteCRMEventQueue(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionDelete, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to delete CRM events", nil, "")
	}

	idParam := r.RequestCtx.UserValue("id")
	idStr, ok := idParam.(string)
	if !ok || idStr == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Missing id", nil, "")
	}
	rowID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	res := a.DB.Where("id = ? AND organization_id = ?", rowID, orgID).Delete(&models.CRMEventQueue{})
	if res.Error != nil {
		a.Log.Error("CRM queue delete failed", "error", res.Error, "id", rowID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete", nil, "")
	}
	if res.RowsAffected == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Event not found", nil, "")
	}
	return r.SendEnvelope(map[string]any{"deleted": true})
}

// InvalidateCRMCache receives POST /api/crm/invalidate-cache from the
// external CRM (Laravel). When a customer is created or updated in the CRM,
// it calls this endpoint so the PBX's in-memory lookup cache is invalidated
// immediately instead of waiting for the TTL to expire.
//
// Authentication: X-iReparo-Api-Key + X-iReparo-Signature (HMAC-SHA256),
// same scheme as the event webhooks but in the reverse direction (CRM → PBX).
//
// Body: { "phone": "<normalized>" }
//
// Response: 200 { "invalidated": true } on success.
func (a *App) InvalidateCRMCache(r *fastglue.Request) error {
	if a.CRM == nil || !a.CRM.Enabled() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "CRM integration is disabled", nil, "")
	}

	// --- Authenticate: API key -------------------------------------------
	apiKey := string(r.RequestCtx.Request.Header.Peek(crm.HeaderAPIKey))
	if apiKey == "" || apiKey != a.Config.Integrations.CRM.APIKey {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid API key", nil, "")
	}

	// --- Authenticate: HMAC signature ------------------------------------
	body := r.RequestCtx.PostBody()
	sig := string(r.RequestCtx.Request.Header.Peek(crm.HeaderSignature))
	ts := string(r.RequestCtx.Request.Header.Peek(crm.HeaderTimestamp))
	if err := crm.VerifySignature(a.Config.Integrations.CRM.WebhookSecret, sig, ts, body); err != nil {
		a.Log.Debug("CRM invalidate-cache: signature verification failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Invalid signature", nil, "")
	}

	// --- Parse body ------------------------------------------------------
	var req struct {
		Phone string `json:"phone"`
	}
	if err := json.Unmarshal(body, &req); err != nil || req.Phone == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Missing or invalid phone field", nil, "")
	}

	// --- Invalidate ------------------------------------------------------
	a.CRM.InvalidateCache(req.Phone)
	a.Log.Info("CRM cache invalidated", "phone", crm.NormalizePhone(req.Phone))

	return r.SendEnvelope(map[string]any{"invalidated": true})
}

// CRMClickToCall receives POST /api/crm/click-to-call from the external CRM.
// It allows a CRM user to initiate an outbound call to a customer via the PBX,
// reusing the same callback-pattern as the internal click-to-call flow:
//
//  1. CRM POSTs { "phone": "<customer>", "agent_email": "<agent>" }
//  2. PBX resolves the agent by email and the contact by phone.
//  3. PBX dials the agent's personal phone first.
//  4. When the agent picks up, Telnyx bridges to the customer.
//
// Authentication: X-iReparo-Api-Key + X-iReparo-Signature (same as events).
//
// The response returns the call_log_id so the CRM can correlate subsequent
// call.* webhook events back to this call.
func (a *App) CRMClickToCall(r *fastglue.Request) error {
	if a.CRM == nil || !a.CRM.Enabled() {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "CRM integration is disabled", nil, "")
	}

	// --- Authenticate: API key -------------------------------------------
	apiKey := string(r.RequestCtx.Request.Header.Peek(crm.HeaderAPIKey))
	if apiKey == "" || apiKey != a.Config.Integrations.CRM.APIKey {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Invalid API key", nil, "")
	}

	// --- Authenticate: HMAC signature ------------------------------------
	body := r.RequestCtx.PostBody()
	sig := string(r.RequestCtx.Request.Header.Peek(crm.HeaderSignature))
	ts := string(r.RequestCtx.Request.Header.Peek(crm.HeaderTimestamp))
	if err := crm.VerifySignature(a.Config.Integrations.CRM.WebhookSecret, sig, ts, body); err != nil {
		a.Log.Debug("CRM click-to-call: signature verification failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "Invalid signature", nil, "")
	}

	// --- Parse body ------------------------------------------------------
	var req struct {
		Phone      string `json:"phone"`       // customer phone (E.164 no +)
		AgentEmail string `json:"agent_email"`  // PBX agent email
		FromNumber string `json:"from_number"`  // optional: override caller ID
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	if req.Phone == "" || req.AgentEmail == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Both phone and agent_email are required", nil, "")
	}

	// --- Resolve agent by email ------------------------------------------
	var agent models.User
	if err := a.DB.Where("email = ?", req.AgentEmail).First(&agent).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Agent not found for email: "+req.AgentEmail, nil, "")
	}
	agentPhone := calling.PhoneToE164(agent.PhoneNumber)
	if agentPhone == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Agent has no phone number configured in their PBX profile", nil, "missing_agent_phone")
	}

	// --- Resolve or create contact by phone ------------------------------
	normalized := crm.NormalizePhone(req.Phone)
	var contact models.Contact
	if err := a.DB.Where("phone_number = ? AND organization_id = ?", normalized, agent.OrganizationID).
		First(&contact).Error; err != nil {
		// Contact does not exist in the PBX yet — create a minimal row so
		// the click-to-call flow has a ContactID to attach the CallLog to.
		contact = models.Contact{
			BaseModel:      models.BaseModel{ID: uuid.New()},
			OrganizationID: agent.OrganizationID,
			PhoneNumber:    normalized,
			ProfileName:    "CRM " + normalized,
		}
		if err := a.DB.Create(&contact).Error; err != nil {
			a.Log.Error("CRM click-to-call: create contact failed", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create contact", nil, "")
		}
	}

	// --- Resolve Telnyx connection ---------------------------------------
	var conn models.TelnyxConnection
	if err := a.DB.Where("organization_id = ?", agent.OrganizationID).First(&conn).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusFailedDependency,
			"No Telnyx connection configured for this organization", nil, "telnyx_not_configured")
	}
	conn.DecryptSecrets(a.Config.App.EncryptionKey)

	// --- Pick FROM number ------------------------------------------------
	fromPhone := calling.PhoneToE164(req.FromNumber)
	if fromPhone == "" {
		var number models.TelnyxNumber
		if err := a.DB.Where("connection_id = ? AND is_active = ?", conn.ID, true).
			Order("created_at ASC").First(&number).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusFailedDependency,
				"No active Telnyx number available as caller ID", nil, "no_outbound_number")
		}
		fromPhone = number.PhoneNumber
	}

	// --- Create CallLog --------------------------------------------------
	now := time.Now().UTC()
	callLog := models.CallLog{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: agent.OrganizationID,
		Channel:        models.CallChannelTelnyxPSTN,
		Direction:      models.CallDirectionOutgoing,
		Status:         models.CallStatusRinging,
		CallerPhone:    fromPhone,
		ContactID:      contact.ID,
		AgentID:        &agent.ID,
		StartedAt:      &now,
	}
	if err := a.DB.Create(&callLog).Error; err != nil {
		a.Log.Error("CRM click-to-call: create CallLog failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create call log", nil, "")
	}

	// --- Emit CRM event for the outbound call ----------------------------
	a.CRMEmitCallEvent(agent.OrganizationID, crm.EventCallRinging, &crm.CallRingingData{
		CallID:        callLog.ID.String(),
		Direction:     "outgoing",
		CallerPhone:   fromPhone,
		CalledPhone:   normalized,
		PBXContactID:  contact.ID.String(),
		ExternalCRMID: contact.ExternalCRMID,
		Channel:       "telnyx_pstn",
	})

	// --- Encode state + dial agent ---------------------------------------
	targetPhone := calling.PhoneToE164(normalized)
	state := &ClickToCallState{
		CallLogID:   callLog.ID.String(),
		TargetPhone: targetPhone,
		FromPhone:   fromPhone,
		OrgID:       agent.OrganizationID.String(),
		InitiatedBy: agent.ID.String(),
	}
	encoded, err := EncodeClickToCallState(state)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to encode call state", nil, "")
	}

	tnx := telnyx.NewClient(conn.APIKey, a.HTTPClient)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dial, err := tnx.Dial(ctx, &telnyx.DialRequest{
		To:           "+" + agentPhone,
		From:         "+" + fromPhone,
		ConnectionID: conn.CallControlAppID,
		ClientState:  encoded,
	})
	if err != nil {
		_ = a.DB.Model(&callLog).Updates(map[string]any{
			"status":        models.CallStatusFailed,
			"error_message": err.Error(),
			"ended_at":      time.Now().UTC(),
		}).Error
		a.Log.Warn("CRM click-to-call: Telnyx Dial failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Telnyx dial failed: "+err.Error(), nil, "")
	}

	_ = a.DB.Model(&callLog).Update("whatsapp_call_id", dial.CallControlID).Error

	a.Log.Info("CRM click-to-call initiated",
		"call_log_id", callLog.ID,
		"agent", req.AgentEmail,
		"customer", normalized)

	return r.SendEnvelope(map[string]any{
		"call_log_id":     callLog.ID,
		"call_control_id": dial.CallControlID,
		"agent_phone":     agentPhone,
		"customer_phone":  normalized,
		"from_number":     fromPhone,
		"status":          "ringing_agent",
	})
}
