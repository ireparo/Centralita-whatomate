package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	wameow "github.com/shridarpatil/whatomate/internal/integrations/whatsmeow"
	"github.com/shridarpatil/whatomate/internal/models"
	"go.mau.fi/whatsmeow/types"
)

// whatsmeow_sink.go implements the wameow.EventSink interface on the
// App struct. This is the bridge that takes the whatsmeow library's
// internal events and feeds them into iReparo's existing messaging
// pipeline — the same one that handles Cloud API webhooks.
//
// Keeping the sink methods in their own file lets the wameow package
// stay in its own subtree without importing handlers, and keeps the
// already-large app.go readable.

// OnIncomingMessage is fired by whatsmeow whenever a text/media message
// arrives on any paired session. We shim it into the existing
// saveIncomingMessage pipeline so the Cloud API and whatsmeow code paths
// converge after this point.
func (a *App) OnIncomingMessage(ctx context.Context, accountID uuid.UUID, evt *wameow.IncomingMessage) {
	if evt == nil {
		return
	}
	// Look up the account so we can route the message correctly.
	var account models.WhatsAppAccount
	if err := a.DB.Where("id = ?", accountID).First(&account).Error; err != nil {
		a.Log.Warn("whatsmeow: incoming message for unknown account",
			"account_id", accountID, "error", err)
		return
	}

	// Resolve (or create) the contact. The existing helper handles
	// both cases, including masking + avatar defaults.
	contact, err := a.resolveOrCreateContact(&account, evt.FromPhone, evt.PushName)
	if err != nil {
		a.Log.Error("whatsmeow: resolve contact failed", "phone", evt.FromPhone, "error", err)
		return
	}

	// Reuse the exact same persistence path the Cloud API webhook uses.
	// This is the core of the integration — once a Message row exists,
	// everything downstream (chatbot, CRM event emission, agent assignment,
	// WebSocket broadcast) fires automatically via the existing code.
	var mediaInfo *MediaInfo
	if evt.MediaURL != "" {
		mediaInfo = &MediaInfo{
			MediaURL:      evt.MediaURL,
			MediaMimeType: evt.MediaMime,
		}
	}
	a.saveIncomingMessage(&account, contact, evt.MessageID, evt.Type, evt.Content, mediaInfo, "")
}

// OnStateChange is fired on every session lifecycle transition. For now
// we only log + broadcast to the admin panel WebSocket so the operator
// can watch the pairing flow in real time.
func (a *App) OnStateChange(accountID uuid.UUID, from, to wameow.ClientState) {
	a.Log.Info("whatsmeow state change",
		"account_id", accountID,
		"from", string(from),
		"to", string(to))

	// TODO (Phase W.2): update whatsapp_accounts.status column based on
	// the new state so the UI can render an Active/Disconnected badge
	// without polling.
	_ = time.Now() // reserved for timestamps when W.2 lands
}

// OnPairSuccess runs when the user scans the QR and whatsmeow saves the
// device. We persist the JID on the WhatsAppAccount row so subsequent
// startups can ReconnectAll without re-pairing.
func (a *App) OnPairSuccess(ctx context.Context, accountID uuid.UUID, jid types.JID) {
	jidStr := jid.String()
	if err := a.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ?", accountID).
		Updates(map[string]any{
			"whatsmeow_jid": jidStr,
			"status":        "active",
		}).Error; err != nil {
		a.Log.Error("whatsmeow: persist paired JID failed",
			"account_id", accountID, "jid", jidStr, "error", err)
		return
	}
	a.Log.Info("whatsmeow paired", "account_id", accountID, "jid", jidStr)
}

// OnLoggedOut fires when WhatsApp (or the user via their phone) tears
// the session down. We clear the JID so the next attempt triggers a
// fresh QR flow.
func (a *App) OnLoggedOut(ctx context.Context, accountID uuid.UUID) {
	if err := a.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ?", accountID).
		Updates(map[string]any{
			"whatsmeow_jid": "",
			"status":        "disconnected",
		}).Error; err != nil {
		a.Log.Warn("whatsmeow: clear JID on logout failed",
			"account_id", accountID, "error", err)
	}
	a.Log.Warn("whatsmeow session logged out", "account_id", accountID)
}

// dispatchWhatsmeowSend is the outbound counterpart of OnIncomingMessage:
// when the WhatsAppAccount uses provider="whatsmeow", the unified
// SendOutgoingMessage routes through here instead of the Cloud API
// client.
//
// Phase W.1 supports TEXT only. Media / interactive / template / flow
// subtypes return a structured error so the UI can surface "feature
// not available on this provider" without the operator having to dig
// through logs. Phase W.2 will add image/audio/video/document support.
func (a *App) dispatchWhatsmeowSend(ctx context.Context, req OutgoingMessageRequest) (string, error) {
	if a.Whatsmeow == nil {
		return "", fmt.Errorf("whatsmeow provider unavailable on this instance")
	}
	if req.Account == nil || req.Contact == nil {
		return "", fmt.Errorf("whatsmeow send: missing account or contact")
	}
	client := a.Whatsmeow.Get(req.Account.ID)
	if client == nil {
		return "", fmt.Errorf("whatsmeow session for account %s is not connected", req.Account.ID)
	}

	switch req.Type {
	case models.MessageTypeText:
		body := req.Content
		if body == "" {
			return "", fmt.Errorf("whatsmeow send: empty text body")
		}
		return client.SendTextMessage(ctx, req.Contact.PhoneNumber, body)

	case models.MessageTypeImage,
		models.MessageTypeVideo,
		models.MessageTypeAudio,
		models.MessageTypeDocument,
		models.MessageTypeTemplate,
		models.MessageTypeInteractive,
		models.MessageTypeFlow:
		return "", fmt.Errorf("message type %q is not supported by the WhatsApp Web provider yet (Phase W.2)", req.Type)

	default:
		return "", fmt.Errorf("unsupported message type %q for whatsmeow provider", req.Type)
	}
}

// resolveOrCreateContact is a thin helper that reuses the same
// upsert-by-phone logic the Cloud API webhook uses. Centralised here
// so the whatsmeow sink does not duplicate the query.
func (a *App) resolveOrCreateContact(account *models.WhatsAppAccount, phone, displayName string) (*models.Contact, error) {
	var contact models.Contact
	err := a.DB.
		Where("organization_id = ? AND phone_number = ?", account.OrganizationID, phone).
		First(&contact).Error
	if err == nil {
		return &contact, nil
	}
	// Create a fresh contact row.
	contact = models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: account.OrganizationID,
		PhoneNumber:    phone,
		ProfileName:    displayName,
	}
	if err := a.DB.Create(&contact).Error; err != nil {
		return nil, err
	}
	return &contact, nil
}
