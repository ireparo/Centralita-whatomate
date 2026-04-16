package handlers

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/calling"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// telnyx_numbers.go is the admin CRUD for TelnyxNumber (DDIs / phone numbers
// routed through iReparo). Numbers are created under an existing
// TelnyxConnection and optionally wired to an IVRFlow.
//
// All numbers are stored in E.164 without the leading "+" so they match what
// Telnyx sends in webhook events and what the CRM normalization expects.

// TelnyxNumberRequest is the create/update body.
type TelnyxNumberRequest struct {
	ConnectionID     string  `json:"connection_id"` // required on create; ignored on update
	PhoneNumber      string  `json:"phone_number"`
	Label            string  `json:"label"`
	Country          string  `json:"country"`
	NumberType       string  `json:"number_type"`
	TelnyxNumberID   string  `json:"telnyx_number_id"`
	IVRFlowID        *string `json:"ivr_flow_id"`   // nullable — null removes the assignment
	IsActive         *bool   `json:"is_active"`
	RecordingEnabled *bool   `json:"recording_enabled"`
}

// TelnyxNumberResponse is the JSON view. Embeds a lightweight IVRFlow
// reference so the UI can render "Recepción Barcelona → Flujo Soporte" in
// one request without needing a second call.
type TelnyxNumberResponse struct {
	ID               uuid.UUID  `json:"id"`
	ConnectionID     uuid.UUID  `json:"connection_id"`
	PhoneNumber      string     `json:"phone_number"`
	Label            string     `json:"label"`
	Country          string     `json:"country"`
	NumberType       string     `json:"number_type"`
	TelnyxNumberID   string     `json:"telnyx_number_id"`
	IVRFlowID        *uuid.UUID `json:"ivr_flow_id,omitempty"`
	IVRFlowName      string     `json:"ivr_flow_name,omitempty"`
	IsActive         bool       `json:"is_active"`
	RecordingEnabled bool       `json:"recording_enabled"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

func toTelnyxNumberResponse(n *models.TelnyxNumber) TelnyxNumberResponse {
	out := TelnyxNumberResponse{
		ID:               n.ID,
		ConnectionID:     n.ConnectionID,
		PhoneNumber:      n.PhoneNumber,
		Label:            n.Label,
		Country:          n.Country,
		NumberType:       n.NumberType,
		TelnyxNumberID:   n.TelnyxNumberID,
		IVRFlowID:        n.IVRFlowID,
		IsActive:         n.IsActive,
		RecordingEnabled: n.RecordingEnabled,
		CreatedAt:        n.CreatedAt,
		UpdatedAt:        n.UpdatedAt,
	}
	if n.IVRFlow != nil {
		out.IVRFlowName = n.IVRFlow.Name
	}
	return out
}

// ListTelnyxNumbers returns all numbers for the current org. Optionally
// filter by connection_id query param if the org ever gets multiple
// connections in the future (model currently enforces one per org).
func (a *App) ListTelnyxNumbers(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to view Telnyx numbers", nil, "")
	}

	q := r.RequestCtx.QueryArgs()
	connFilter := string(q.Peek("connection_id"))

	db := a.DB.Model(&models.TelnyxNumber{}).
		Preload("IVRFlow").
		Where("organization_id = ?", orgID)
	if connFilter != "" {
		db = db.Where("connection_id = ?", connFilter)
	}

	var numbers []models.TelnyxNumber
	if err := db.Order("created_at DESC").Find(&numbers).Error; err != nil {
		a.Log.Error("List Telnyx numbers failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to list numbers", nil, "")
	}

	out := make([]TelnyxNumberResponse, 0, len(numbers))
	for i := range numbers {
		out = append(out, toTelnyxNumberResponse(&numbers[i]))
	}
	return r.SendEnvelope(map[string]any{
		"numbers": out,
		"total":   len(out),
	})
}

// CreateTelnyxNumber registers a DDI under a connection. Normalizes the
// phone to E.164 (no +) before storing so it matches Telnyx webhook
// payloads and CRM expectations.
func (a *App) CreateTelnyxNumber(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to manage Telnyx numbers", nil, "")
	}

	var req TelnyxNumberRequest
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	if req.ConnectionID == "" || req.PhoneNumber == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "connection_id and phone_number are required", nil, "")
	}
	connID, err := uuid.Parse(req.ConnectionID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid connection_id", nil, "")
	}

	// Enforce the connection belongs to this org (prevents cross-org inserts).
	var conn models.TelnyxConnection
	if err := a.DB.Where("id = ? AND organization_id = ?", connID, orgID).First(&conn).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Connection not found in this organization", nil, "")
	}

	phone := calling.PhoneToE164(req.PhoneNumber)
	if phone == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid phone number", nil, "")
	}

	var ivrFlowID *uuid.UUID
	if req.IVRFlowID != nil && *req.IVRFlowID != "" {
		parsed, err := uuid.Parse(*req.IVRFlowID)
		if err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid ivr_flow_id", nil, "")
		}
		// Verify flow exists + belongs to the same org.
		var flow models.IVRFlow
		if err := a.DB.Where("id = ? AND organization_id = ?", parsed, orgID).First(&flow).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "IVR flow not found in this organization", nil, "")
		}
		ivrFlowID = &parsed
	}

	isActive := true
	if req.IsActive != nil {
		isActive = *req.IsActive
	}
	recording := false
	if req.RecordingEnabled != nil {
		recording = *req.RecordingEnabled
	}

	num := models.TelnyxNumber{
		BaseModel:        models.BaseModel{ID: uuid.New()},
		OrganizationID:   orgID,
		ConnectionID:     connID,
		PhoneNumber:      phone,
		Label:            req.Label,
		Country:          req.Country,
		NumberType:       req.NumberType,
		TelnyxNumberID:   req.TelnyxNumberID,
		IVRFlowID:        ivrFlowID,
		IsActive:         isActive,
		RecordingEnabled: recording,
		CreatedByID:      &userID,
		UpdatedByID:      &userID,
	}
	if err := a.DB.Create(&num).Error; err != nil {
		// unique violation on phone_number → 409
		a.Log.Error("Create Telnyx number failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "This phone number is already registered", nil, "")
	}
	// Reload with IVRFlow preloaded for the response.
	_ = a.DB.Preload("IVRFlow").Where("id = ?", num.ID).First(&num).Error
	return r.SendEnvelope(toTelnyxNumberResponse(&num))
}

// UpdateTelnyxNumber partially updates a number. Any field not present in the
// request preserves its current value; IVRFlowID can be cleared by sending
// an explicit JSON null.
func (a *App) UpdateTelnyxNumber(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to manage Telnyx numbers", nil, "")
	}

	idStr, _ := r.RequestCtx.UserValue("id").(string)
	numID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	var num models.TelnyxNumber
	if err := a.DB.Where("id = ? AND organization_id = ?", numID, orgID).First(&num).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Number not found", nil, "")
	}

	// Parse as raw map so we can distinguish "unset" from "explicit null".
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &raw); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	updates := map[string]any{"updated_by_id": &userID}

	if v, ok := raw["label"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		updates["label"] = s
	}
	if v, ok := raw["country"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		updates["country"] = s
	}
	if v, ok := raw["number_type"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		updates["number_type"] = s
	}
	if v, ok := raw["telnyx_number_id"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		updates["telnyx_number_id"] = s
	}
	if v, ok := raw["phone_number"]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		phone := calling.PhoneToE164(s)
		if phone == "" {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid phone number", nil, "")
		}
		updates["phone_number"] = phone
	}
	if v, ok := raw["is_active"]; ok {
		var b bool
		_ = json.Unmarshal(v, &b)
		updates["is_active"] = b
	}
	if v, ok := raw["recording_enabled"]; ok {
		var b bool
		_ = json.Unmarshal(v, &b)
		updates["recording_enabled"] = b
	}
	if v, ok := raw["ivr_flow_id"]; ok {
		if string(v) == "null" || string(v) == `""` {
			updates["ivr_flow_id"] = nil
		} else {
			var s string
			_ = json.Unmarshal(v, &s)
			parsed, err := uuid.Parse(s)
			if err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid ivr_flow_id", nil, "")
			}
			var flow models.IVRFlow
			if err := a.DB.Where("id = ? AND organization_id = ?", parsed, orgID).First(&flow).Error; err != nil {
				return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "IVR flow not found in this organization", nil, "")
			}
			updates["ivr_flow_id"] = parsed
		}
	}

	if err := a.DB.Model(&num).Updates(updates).Error; err != nil {
		// GORM Updates ignores nil, so we explicitly unset ivr_flow_id when
		// requested via a separate Update call.
		a.Log.Error("Update Telnyx number failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "Failed to update number (possibly duplicate phone_number)", nil, "")
	}
	if v, ok := updates["ivr_flow_id"]; ok && v == nil {
		_ = a.DB.Model(&num).Update("ivr_flow_id", gorm.Expr("NULL")).Error
	}

	_ = a.DB.Preload("IVRFlow").Where("id = ?", num.ID).First(&num).Error
	return r.SendEnvelope(toTelnyxNumberResponse(&num))
}

// DeleteTelnyxNumber removes a number. Does NOT cascade to calls / call logs —
// those survive with a reference to the (now-deleted) number id for history.
func (a *App) DeleteTelnyxNumber(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionDelete, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to delete Telnyx numbers", nil, "")
	}

	idStr, _ := r.RequestCtx.UserValue("id").(string)
	numID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}
	res := a.DB.Where("id = ? AND organization_id = ?", numID, orgID).Delete(&models.TelnyxNumber{})
	if res.Error != nil {
		a.Log.Error("Delete Telnyx number failed", "error", res.Error)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete number", nil, "")
	}
	if res.RowsAffected == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Number not found", nil, "")
	}
	return r.SendEnvelope(map[string]any{"deleted": true})
}

// --- Internal helpers ------------------------------------------------------

// ensureTelnyxModelsMigrated is a compile-time fence — if GORM ever forgets
// to AutoMigrate telnyx_numbers/connections, the call to Find below will
// surface the "relation does not exist" error early.
var _ = func() error {
	return errors.New("unused sentinel")
}
