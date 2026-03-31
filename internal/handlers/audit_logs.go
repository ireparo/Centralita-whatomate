package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// AuditLogResponse represents an audit log entry in API response
type AuditLogResponse struct {
	ID           uuid.UUID         `json:"id"`
	ResourceType string            `json:"resource_type"`
	ResourceID   uuid.UUID         `json:"resource_id"`
	UserName     string            `json:"user_name"`
	Action       models.AuditAction `json:"action"`
	Changes      models.JSONBArray `json:"changes"`
	CreatedAt    time.Time         `json:"created_at"`
}

// ListAuditLogs returns audit logs for a specific resource
func (a *App) ListAuditLogs(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}

	if err := a.requirePermission(r, userID, models.ResourceSettingsGeneral, models.ActionRead); err != nil {
		return nil
	}

	resourceType := string(r.RequestCtx.QueryArgs().Peek("resource_type"))
	resourceIDStr := string(r.RequestCtx.QueryArgs().Peek("resource_id"))

	if resourceType == "" || resourceIDStr == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"resource_type and resource_id are required", nil, "")
	}

	resourceID, err := uuid.Parse(resourceIDStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"Invalid resource_id", nil, "")
	}

	pg := parsePagination(r)

	var logs []models.AuditLog
	var total int64

	baseQuery := a.DB.Where(
		"organization_id = ? AND resource_type = ? AND resource_id = ?",
		orgID, resourceType, resourceID,
	)
	baseQuery.Model(&models.AuditLog{}).Count(&total)

	if err := pg.Apply(baseQuery.Order("created_at DESC")).Find(&logs).Error; err != nil {
		a.Log.Error("Failed to list audit logs", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError,
			"Failed to list audit logs", nil, "")
	}

	response := make([]AuditLogResponse, len(logs))
	for i, l := range logs {
		response[i] = AuditLogResponse{
			ID:           l.ID,
			ResourceType: l.ResourceType,
			ResourceID:   l.ResourceID,
			UserName:     l.UserName,
			Action:       l.Action,
			Changes:      l.Changes,
			CreatedAt:    l.CreatedAt,
		}
	}

	return r.SendEnvelope(map[string]any{
		"audit_logs": response,
		"total":      total,
		"page":       pg.Page,
		"limit":      pg.Limit,
	})
}
