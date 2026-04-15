package handlers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	wameow "github.com/shridarpatil/whatomate/internal/integrations/whatsmeow"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// whatsmeow_groups.go exposes the group-admin HTTP surface for Phase W.5.
// All routes are scoped to an existing group Contact in the current org
// (or to an account for the create-group endpoint).
//
// Routes (registered in main.go):
//
//	POST   /api/accounts/{id}/whatsmeow/groups
//	       body: { subject: "Ventas Barcelona",
//	               participant_phones: ["34666...", "34677..."] }
//	       Creates a new WhatsApp group + a matching Contact row.
//
//	GET    /api/contacts/{id}/whatsmeow/group
//	       Returns the live GroupInfoSummary (participants + roles)
//	       fetched from WhatsApp's servers. Used by the frontend
//	       group-admin panel.
//
//	POST   /api/contacts/{id}/whatsmeow/group/participants
//	       body: { action: "add"|"remove"|"promote"|"demote",
//	               phones: ["34666...", ...] }
//	       Applies the action and returns { accepted: [...] }.
//
//	PUT    /api/contacts/{id}/whatsmeow/group/subject
//	       body: { subject: "Nuevo nombre" }
//	       Renames the group.
//
//	POST   /api/contacts/{id}/whatsmeow/group/leave
//	       The paired account leaves the group. The Contact row stays
//	       so historical messages are preserved.
//
// Permissions: contacts:write for all actions. Fine-grained per-action
// permissions can be layered in later if needed.

// CreateGroupRequest is the POST body for the create-group endpoint.
type CreateGroupRequest struct {
	Subject           string   `json:"subject"`
	ParticipantPhones []string `json:"participant_phones"`
}

// UpdateParticipantsRequest is the body for the shared participants
// endpoint — one shape for add / remove / promote / demote.
type UpdateParticipantsRequest struct {
	Action string   `json:"action"` // add | remove | promote | demote
	Phones []string `json:"phones"`
}

// CreateWhatsmeowGroup handles POST /api/accounts/{id}/whatsmeow/groups.
func (a *App) CreateWhatsmeowGroup(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to create groups", nil, "")
	}
	if a.Whatsmeow == nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "WhatsApp Web provider is not available", nil, "")
	}

	accountID, errResp := a.loadWhatsmeowAccount(r, orgID)
	if errResp != nil {
		return errResp
	}

	var req CreateGroupRequest
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	if req.Subject == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "subject is required", nil, "")
	}
	if len(req.ParticipantPhones) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "at least one participant_phone is required", nil, "")
	}

	client := a.Whatsmeow.Get(accountID)
	if client == nil {
		return r.SendErrorEnvelope(fasthttp.StatusFailedDependency, "WhatsApp session not connected", nil, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	info, err := client.CreateGroup(ctx, req.Subject, req.ParticipantPhones)
	if err != nil {
		a.Log.Warn("whatsmeow create group failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Failed to create group: "+err.Error(), nil, "")
	}

	// Upsert a Contact row for the fresh group so it shows up in the
	// chat list without waiting for the first inbound message.
	var account models.WhatsAppAccount
	if err := a.DB.Where("id = ?", accountID).First(&account).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load account", nil, "")
	}
	jid, _ := wameow.ParseJID(info.JID)
	groupPhone := jid.User
	contact := models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		PhoneNumber:    groupPhone,
		ProfileName:    info.Subject,
		IsGroup:        true,
		GroupJID:       info.JID,
		GroupSubject:   info.Subject,
	}
	_ = a.DB.
		Where("organization_id = ? AND is_group = ? AND group_jid = ?", orgID, true, info.JID).
		FirstOrCreate(&contact).Error

	return r.SendEnvelope(map[string]any{
		"group":      info,
		"contact_id": contact.ID.String(),
	})
}

// GetWhatsmeowGroupInfo handles GET /api/contacts/{id}/whatsmeow/group.
func (a *App) GetWhatsmeowGroupInfo(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission", nil, "")
	}
	contact, accountID, errResp := a.loadGroupContactAndAccount(r, orgID)
	if errResp != nil {
		return errResp
	}
	client := a.Whatsmeow.Get(accountID)
	if client == nil {
		return r.SendErrorEnvelope(fasthttp.StatusFailedDependency, "WhatsApp session not connected", nil, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := client.GetGroupInfo(ctx, contact.GroupJID)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Failed to fetch group info: "+err.Error(), nil, "")
	}

	// Keep Contact.GroupSubject in sync in case it changed remotely.
	if info.Subject != "" && info.Subject != contact.GroupSubject {
		_ = a.DB.Model(contact).Updates(map[string]any{
			"group_subject": info.Subject,
			"profile_name":  info.Subject,
		}).Error
	}
	return r.SendEnvelope(info)
}

// UpdateWhatsmeowGroupParticipants handles POST
// /api/contacts/{id}/whatsmeow/group/participants. One shape for the
// four actions — same as whatsmeow's own API.
func (a *App) UpdateWhatsmeowGroupParticipants(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission", nil, "")
	}
	contact, accountID, errResp := a.loadGroupContactAndAccount(r, orgID)
	if errResp != nil {
		return errResp
	}
	client := a.Whatsmeow.Get(accountID)
	if client == nil {
		return r.SendErrorEnvelope(fasthttp.StatusFailedDependency, "WhatsApp session not connected", nil, "")
	}

	var req UpdateParticipantsRequest
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid request body", nil, "")
	}
	action := wameow.ParticipantChange(req.Action)
	switch action {
	case wameow.ParticipantChangeAdd,
		wameow.ParticipantChangeRemove,
		wameow.ParticipantChangePromote,
		wameow.ParticipantChangeDemote:
		// OK
	default:
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "action must be one of: add, remove, promote, demote", nil, "")
	}
	if len(req.Phones) == 0 {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "phones is required", nil, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	accepted, err := client.UpdateGroupParticipants(ctx, contact.GroupJID, req.Phones, action)
	if err != nil {
		a.Log.Warn("whatsmeow group participants update failed", "error", err, "action", action)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, err.Error(), nil, "")
	}
	return r.SendEnvelope(map[string]any{
		"action":   req.Action,
		"accepted": accepted,
	})
}

// SetWhatsmeowGroupSubject handles PUT /api/contacts/{id}/whatsmeow/group/subject.
func (a *App) SetWhatsmeowGroupSubject(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission", nil, "")
	}
	contact, accountID, errResp := a.loadGroupContactAndAccount(r, orgID)
	if errResp != nil {
		return errResp
	}
	client := a.Whatsmeow.Get(accountID)
	if client == nil {
		return r.SendErrorEnvelope(fasthttp.StatusFailedDependency, "WhatsApp session not connected", nil, "")
	}

	var req struct {
		Subject string `json:"subject"`
	}
	if err := json.Unmarshal(r.RequestCtx.PostBody(), &req); err != nil || req.Subject == "" {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "subject is required", nil, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.SetGroupSubject(ctx, contact.GroupJID, req.Subject); err != nil {
		a.Log.Warn("whatsmeow set group subject failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, err.Error(), nil, "")
	}
	// Mirror on the Contact row so the UI reflects immediately.
	_ = a.DB.Model(contact).Updates(map[string]any{
		"group_subject": req.Subject,
		"profile_name":  req.Subject,
	}).Error
	return r.SendEnvelope(map[string]any{"subject": req.Subject})
}

// LeaveWhatsmeowGroup handles POST /api/contacts/{id}/whatsmeow/group/leave.
func (a *App) LeaveWhatsmeowGroup(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceContacts, models.ActionDelete, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission", nil, "")
	}
	contact, accountID, errResp := a.loadGroupContactAndAccount(r, orgID)
	if errResp != nil {
		return errResp
	}
	client := a.Whatsmeow.Get(accountID)
	if client == nil {
		return r.SendErrorEnvelope(fasthttp.StatusFailedDependency, "WhatsApp session not connected", nil, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.LeaveGroup(ctx, contact.GroupJID); err != nil {
		a.Log.Warn("whatsmeow leave group failed", "error", err)
		return r.SendErrorEnvelope(fasthttp.StatusBadGateway, err.Error(), nil, "")
	}
	return r.SendEnvelope(map[string]any{"status": "left"})
}

// loadGroupContactAndAccount resolves the URL id param to a Contact
// (must exist + be a group + belong to the org) and the WhatsAppAccount
// that owns it (must be a whatsmeow-provider account with a connected
// session). Returns a non-nil error via r.SendErrorEnvelope on any
// failure; caller only needs to `return errResp` up the stack.
func (a *App) loadGroupContactAndAccount(r *fastglue.Request, orgID uuid.UUID) (contact *models.Contact, accountID uuid.UUID, errResp error) {
	idStr, _ := r.RequestCtx.UserValue("id").(string)
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		return nil, uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact id", nil, "")
	}
	var c models.Contact
	if err := a.DB.Where("id = ? AND organization_id = ?", contactID, orgID).First(&c).Error; err != nil {
		return nil, uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
	}
	if !c.IsGroup || c.GroupJID == "" {
		return nil, uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusBadRequest, "This contact is not a WhatsApp group", nil, "")
	}
	if c.WhatsAppAccount == "" {
		return nil, uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Group contact has no WhatsApp account", nil, "")
	}
	account, err := a.resolveWhatsAppAccount(orgID, c.WhatsAppAccount)
	if err != nil {
		return nil, uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to load account", nil, "")
	}
	if account.Provider != wameow.Provider {
		return nil, uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Group contact is not on the whatsmeow provider", nil, "")
	}
	if a.Whatsmeow == nil {
		return nil, uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "WhatsApp Web provider is not available", nil, "")
	}
	return &c, account.ID, nil
}
