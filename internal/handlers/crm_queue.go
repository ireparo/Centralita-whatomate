package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// CRMQueueResponse is the public-facing shape of a CRMEventQueue row.
// Payload is intentionally NOT included by default (can be large and
// not needed for the list view); use the detail endpoint if the
// operator needs to inspect it.
type CRMQueueResponse struct {
	ID            uuid.UUID  `json:"id"`
	EventType     string     `json:"event_type"`
	Endpoint      string     `json:"endpoint"`
	Status        string     `json:"status"`
	AttemptCount  int        `json:"attempt_count"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	DeliveredAt   *time.Time `json:"delivered_at,omitempty"`
	Timestamp     int64      `json:"timestamp"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type CRMQueueDetailResponse struct {
	CRMQueueResponse
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// ListCRMQueue returns CRM event queue rows for the current org.
// GET /api/crm-queue?status=pending|delivered|dead_letter&event_type=...
//
// Requires settings.general:read — DLQ visibility is an ops concern.
func (a *App) ListCRMQueue(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if err := a.requirePermission(r, userID, models.ResourceSettingsGeneral, models.ActionRead); err != nil {
		return nil
	}

	q := a.DB.Model(&models.CRMEventQueue{}).Where("organization_id = ?", orgID)

	if v := string(r.RequestCtx.QueryArgs().Peek("status")); v != "" {
		q = q.Where("status = ?", v)
	}
	if v := string(r.RequestCtx.QueryArgs().Peek("event_type")); v != "" {
		q = q.Where("event_type = ?", v)
	}

	pg := parsePagination(r)

	var rows []models.CRMEventQueue
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to count", nil, "")
	}
	if err := q.Order("created_at DESC").
		Limit(pg.Limit).Offset(pg.Offset).
		Find(&rows).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list", nil, "")
	}

	out := make([]CRMQueueResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, toCRMQueueResponse(&row))
	}

	// Also return summary counts by status so the UI can render tabs
	// with badges without a second request.
	type statusCount struct {
		Status string
		Count  int64
	}
	var counts []statusCount
	_ = a.DB.Model(&models.CRMEventQueue{}).
		Where("organization_id = ?", orgID).
		Select("status, COUNT(*) as count").
		Group("status").
		Scan(&counts).Error
	summary := map[string]int64{"pending": 0, "delivered": 0, "dead_letter": 0}
	for _, c := range counts {
		summary[c.Status] = c.Count
	}

	return r.SendEnvelope(map[string]any{
		"queue":   out,
		"total":   total,
		"summary": summary,
		"page":    pg.Page,
		"limit":   pg.Limit,
	})
}

// GetCRMQueueItem returns a single queue row including its full payload.
// GET /api/crm-queue/:id
func (a *App) GetCRMQueueItem(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if err := a.requirePermission(r, userID, models.ResourceSettingsGeneral, models.ActionRead); err != nil {
		return nil
	}

	id, err := uuid.Parse(r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	var row models.CRMEventQueue
	if err := a.DB.Where("organization_id = ? AND id = ?", orgID, id).First(&row).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Not found", nil, "")
	}

	return r.SendEnvelope(CRMQueueDetailResponse{
		CRMQueueResponse: toCRMQueueResponse(&row),
		Payload:          row.Payload,
		Signature:        row.Signature,
	})
}

// ReplayCRMQueueItem resets a dead-lettered (or failing-pending) event
// back to "pending" with attempt_count=0 and next_attempt_at=now so the
// queue worker picks it up on its next tick.
// POST /api/crm-queue/:id/replay
//
// Requires settings.general:write — this re-sends data externally.
func (a *App) ReplayCRMQueueItem(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if err := a.requirePermission(r, userID, models.ResourceSettingsGeneral, models.ActionWrite); err != nil {
		return nil
	}

	id, err := uuid.Parse(r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	var row models.CRMEventQueue
	if err := a.DB.Where("organization_id = ? AND id = ?", orgID, id).First(&row).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Not found", nil, "")
	}

	if row.Status == "delivered" {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Already delivered", nil, "")
	}

	now := time.Now().UTC()
	if err := a.DB.Model(&row).Updates(map[string]any{
		"status":          "pending",
		"attempt_count":   0,
		"next_attempt_at": now,
		"last_error":      "",
	}).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to replay", nil, "")
	}

	a.Log.Info("CRM queue: manual replay",
		"id", row.ID, "event_type", row.EventType, "user_id", userID)

	_ = a.DB.Where("id = ?", id).First(&row)
	return r.SendEnvelope(toCRMQueueResponse(&row))
}

// DiscardCRMQueueItem hard-deletes a queue row. Used when the event is
// obsolete (e.g. a call that no longer makes sense to mirror) or when
// manual replay is futile (CRM permanently rejects the payload).
// DELETE /api/crm-queue/:id
func (a *App) DiscardCRMQueueItem(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if err := a.requirePermission(r, userID, models.ResourceSettingsGeneral, models.ActionWrite); err != nil {
		return nil
	}

	id, err := uuid.Parse(r.RequestCtx.UserValue("id").(string))
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	res := a.DB.Where("organization_id = ? AND id = ?", orgID, id).Delete(&models.CRMEventQueue{})
	if res.Error != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to discard", nil, "")
	}
	if res.RowsAffected == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Not found", nil, "")
	}

	a.Log.Info("CRM queue: manual discard", "id", id, "user_id", userID)

	return r.SendEnvelope(map[string]any{"discarded": true})
}

func toCRMQueueResponse(row *models.CRMEventQueue) CRMQueueResponse {
	return CRMQueueResponse{
		ID:            row.ID,
		EventType:     row.EventType,
		Endpoint:      row.Endpoint,
		Status:        row.Status,
		AttemptCount:  row.AttemptCount,
		NextAttemptAt: row.NextAttemptAt,
		LastError:     row.LastError,
		DeliveredAt:   row.DeliveredAt,
		Timestamp:     row.Timestamp,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
}
