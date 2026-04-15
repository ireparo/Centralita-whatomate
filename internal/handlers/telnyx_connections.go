package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/crypto"
	"github.com/shridarpatil/whatomate/internal/integrations/telnyx"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
	"gorm.io/gorm"
)

// telnyx_connections.go is the admin CRUD for TelnyxConnection — the credential
// + configuration row that identifies an organization's Telnyx account.
//
// The model enforces a uniqueIndex on organization_id so there is at most one
// connection per org. Handlers reflect that: a Create call on an org that
// already has a connection returns 409 Conflict.
//
// Secrets (APIKey, PublicKey) are encrypted at rest with the app encryption
// key. They are NEVER returned in responses — the response DTO strips them
// (and the GORM `json:"-"` tag provides a second layer of defense).

// TelnyxConnectionRequest is the create/update body.
type TelnyxConnectionRequest struct {
	Label             string `json:"label"`
	APIKey            string `json:"api_key"`
	PublicKey         string `json:"public_key"`
	CallControlAppID  string `json:"call_control_app_id"`
	OutboundProfileID string `json:"outbound_profile_id"`
}

// TelnyxConnectionResponse is the safe view returned to clients. Secrets are
// surfaced only as booleans (HasAPIKey / HasPublicKey) so the UI can show
// "•••••• · Update" affordances without ever exposing the plaintext.
type TelnyxConnectionResponse struct {
	ID                uuid.UUID  `json:"id"`
	Label             string     `json:"label"`
	CallControlAppID  string     `json:"call_control_app_id"`
	OutboundProfileID string     `json:"outbound_profile_id"`
	Status            string     `json:"status"`
	HasAPIKey         bool       `json:"has_api_key"`
	HasPublicKey      bool       `json:"has_public_key"`
	LastVerifiedAt    *time.Time `json:"last_verified_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	NumbersCount      int        `json:"numbers_count"`
}

func toTelnyxConnectionResponse(c *models.TelnyxConnection, numbersCount int) TelnyxConnectionResponse {
	return TelnyxConnectionResponse{
		ID:                c.ID,
		Label:             c.Label,
		CallControlAppID:  c.CallControlAppID,
		OutboundProfileID: c.OutboundProfileID,
		Status:            c.Status,
		HasAPIKey:         c.APIKey != "",
		HasPublicKey:      c.PublicKey != "",
		LastVerifiedAt:    c.LastVerifiedAt,
		CreatedAt:         c.CreatedAt,
		UpdatedAt:         c.UpdatedAt,
		NumbersCount:      numbersCount,
	}
}

// GetTelnyxConnection returns the single connection for the current org, or
// 404 if none exists. Used by the admin UI to show the current state + the
// "Add connection" empty state.
func (a *App) GetTelnyxConnection(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to view the Telnyx connection", nil, "")
	}

	var conn models.TelnyxConnection
	if err := a.DB.Where("organization_id = ?", orgID).First(&conn).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "No Telnyx connection configured", nil, "")
		}
		a.Log.Error("Get Telnyx connection failed", "error", err, "org_id", orgID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load connection", nil, "")
	}
	var numbersCount int64
	a.DB.Model(&models.TelnyxNumber{}).Where("connection_id = ?", conn.ID).Count(&numbersCount)
	return r.SendEnvelope(toTelnyxConnectionResponse(&conn, int(numbersCount)))
}

// CreateTelnyxConnection creates the org's Telnyx connection. Validates the
// credentials via Telnyx's /me endpoint before persisting so a bad key is
// rejected at submit time rather than silently stored and then failing on
// the first incoming call.
func (a *App) CreateTelnyxConnection(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to manage Telnyx connections", nil, "")
	}

	var req TelnyxConnectionRequest
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	if req.Label == "" || req.APIKey == "" || req.CallControlAppID == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Label, api_key and call_control_app_id are required", nil, "")
	}

	// One connection per org.
	var existing models.TelnyxConnection
	if err := a.DB.Where("organization_id = ?", orgID).First(&existing).Error; err == nil {
		return r.SendErrorEnvelope(fasthttp.StatusConflict, "A Telnyx connection already exists for this organization", nil, "")
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		a.Log.Error("Create Telnyx connection: existence check failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create connection", nil, "")
	}

	// Validate credentials before storing.
	if err := a.verifyTelnyxCredentials(req.APIKey); err != nil {
		a.Log.Info("Telnyx credential verification failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Telnyx credential verification failed: "+err.Error(), nil, "")
	}

	encKey := a.Config.App.EncryptionKey
	encAPIKey, err := crypto.Encrypt(req.APIKey, encKey)
	if err != nil {
		a.Log.Error("Encrypt API key failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create connection", nil, "")
	}
	var encPublicKey string
	if req.PublicKey != "" {
		encPublicKey, err = crypto.Encrypt(req.PublicKey, encKey)
		if err != nil {
			a.Log.Error("Encrypt public key failed", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create connection", nil, "")
		}
	}

	now := time.Now().UTC()
	conn := models.TelnyxConnection{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		OrganizationID:    orgID,
		Label:             req.Label,
		APIKey:            encAPIKey,
		PublicKey:         encPublicKey,
		CallControlAppID:  req.CallControlAppID,
		OutboundProfileID: req.OutboundProfileID,
		Status:            "active",
		LastVerifiedAt:    &now,
		CreatedByID:       &userID,
		UpdatedByID:       &userID,
	}
	if err := a.DB.Create(&conn).Error; err != nil {
		a.Log.Error("Create Telnyx connection failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to create connection", nil, "")
	}
	return r.SendEnvelope(toTelnyxConnectionResponse(&conn, 0))
}

// UpdateTelnyxConnection updates the connection. Fields left empty in the
// request preserve their current values — this lets the UI show "••••••"
// placeholders for the secrets and only re-send them when the admin
// explicitly rotates them.
func (a *App) UpdateTelnyxConnection(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to manage Telnyx connections", nil, "")
	}

	idStr, _ := r.RequestCtx.UserValue("id").(string)
	connID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	var conn models.TelnyxConnection
	if err := a.DB.Where("id = ? AND organization_id = ?", connID, orgID).First(&conn).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Connection not found", nil, "")
	}

	var req TelnyxConnectionRequest
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}

	encKey := a.Config.App.EncryptionKey
	updates := map[string]any{
		"updated_by_id": &userID,
	}
	if req.Label != "" {
		updates["label"] = req.Label
	}
	if req.CallControlAppID != "" {
		updates["call_control_app_id"] = req.CallControlAppID
	}
	// OutboundProfileID is allowed to be cleared (empty string = remove).
	if req.OutboundProfileID != conn.OutboundProfileID {
		updates["outbound_profile_id"] = req.OutboundProfileID
	}
	if req.APIKey != "" {
		// Verify the new key before saving.
		if err := a.verifyTelnyxCredentials(req.APIKey); err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Telnyx credential verification failed: "+err.Error(), nil, "")
		}
		enc, err := crypto.Encrypt(req.APIKey, encKey)
		if err != nil {
			a.Log.Error("Encrypt API key failed", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update connection", nil, "")
		}
		updates["api_key"] = enc
		now := time.Now().UTC()
		updates["last_verified_at"] = now
	}
	if req.PublicKey != "" {
		enc, err := crypto.Encrypt(req.PublicKey, encKey)
		if err != nil {
			a.Log.Error("Encrypt public key failed", "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update connection", nil, "")
		}
		updates["public_key"] = enc
	}
	if err := a.DB.Model(&conn).Updates(updates).Error; err != nil {
		a.Log.Error("Update Telnyx connection failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to update connection", nil, "")
	}

	// Reload so the response has the latest timestamps + updated fields.
	_ = a.DB.Where("id = ?", conn.ID).First(&conn).Error
	var numbersCount int64
	a.DB.Model(&models.TelnyxNumber{}).Where("connection_id = ?", conn.ID).Count(&numbersCount)
	return r.SendEnvelope(toTelnyxConnectionResponse(&conn, int(numbersCount)))
}

// DeleteTelnyxConnection removes the org's Telnyx connection AND its associated
// numbers (cascade). Use with care — any incoming call on those numbers will
// be rejected until a new connection is configured.
func (a *App) DeleteTelnyxConnection(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionDelete, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to delete Telnyx connections", nil, "")
	}

	idStr, _ := r.RequestCtx.UserValue("id").(string)
	connID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid id", nil, "")
	}

	// Cascade delete numbers first, then the connection. Wrap in a tx so a
	// partial failure does not leave orphans.
	err = a.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("connection_id = ? AND organization_id = ?", connID, orgID).
			Delete(&models.TelnyxNumber{}).Error; err != nil {
			return err
		}
		res := tx.Where("id = ? AND organization_id = ?", connID, orgID).
			Delete(&models.TelnyxConnection{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Connection not found", nil, "")
		}
		a.Log.Error("Delete Telnyx connection failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to delete connection", nil, "")
	}
	return r.SendEnvelope(map[string]any{"deleted": true})
}

// TestTelnyxConnection is a standalone validation endpoint — the admin UI
// calls it with the currently-typed API key (before save) to show a green /
// red indicator. Accepts either a saved connection id (and pulls the
// encrypted key) OR a raw api_key in the body.
func (a *App) TestTelnyxConnection(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceSettingsGeneral, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission", nil, "")
	}

	var req struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(r.RequestCtx.PostBody(), &req)

	apiKey := req.APIKey
	if apiKey == "" {
		// Fall back to the saved connection's key.
		var conn models.TelnyxConnection
		if err := a.DB.Where("organization_id = ?", orgID).First(&conn).Error; err != nil {
			return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "No api_key provided and no saved connection found", nil, "")
		}
		conn.DecryptSecrets(a.Config.App.EncryptionKey)
		apiKey = conn.APIKey
	}

	if err := a.verifyTelnyxCredentials(apiKey); err != nil {
		return r.SendEnvelope(map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
	}
	return r.SendEnvelope(map[string]any{"ok": true})
}

// verifyTelnyxCredentials calls the Telnyx /me endpoint with the supplied API
// key and returns an error if the call fails or the response is unexpected.
// 3s timeout — fast enough for a UI "test" button, long enough for Telnyx's
// worst-case latency.
func (a *App) verifyTelnyxCredentials(apiKey string) error {
	client := telnyx.NewClient(apiKey, a.HTTPClient)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Whoami(ctx); err != nil {
		return err
	}
	return nil
}
