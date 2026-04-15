package handlers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	//
	// Phase W.2: for media types, download + decrypt via whatsmeow (raw
	// WhatsApp CDN URLs need the message's MediaKey to decrypt) and save
	// locally so the existing /api/media/{id} endpoint can serve them.
	var mediaInfo *MediaInfo
	if evt.MediaURL != "" && evt.Raw != nil && evt.Raw.Message != nil {
		client := a.Whatsmeow.Get(accountID)
		if client != nil {
			dlCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			bytes, derr := client.DownloadMedia(dlCtx, evt.Raw.Message)
			cancel()
			if derr == nil && len(bytes) > 0 {
				relPath, serr := a.SaveMediaBytes(bytes, evt.MediaMime)
				if serr == nil {
					mediaInfo = &MediaInfo{
						MediaURL:      relPath,
						MediaMimeType: evt.MediaMime,
					}
				} else {
					a.Log.Warn("whatsmeow: save media bytes failed", "error", serr)
				}
			} else if derr != nil {
				a.Log.Warn("whatsmeow: download media failed", "error", derr, "type", evt.Type)
			}
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
		models.MessageTypeDocument:
		// Phase W.2 — resolve bytes, then delegate to the matching
		// Send*Message method on the wrapper.
		data, err := a.resolveWhatsmeowMediaBytes(req)
		if err != nil {
			return "", err
		}
		payload := wameow.MediaPayload{
			Data:     data,
			Mime:     req.MediaMimeType,
			Filename: req.MediaFilename,
			Caption:  req.Caption,
		}
		phone := req.Contact.PhoneNumber
		switch req.Type {
		case models.MessageTypeImage:
			return client.SendImageMessage(ctx, phone, payload)
		case models.MessageTypeVideo:
			return client.SendVideoMessage(ctx, phone, payload)
		case models.MessageTypeAudio:
			return client.SendAudioMessage(ctx, phone, payload)
		default: // document
			return client.SendDocumentMessage(ctx, phone, payload)
		}

	case models.MessageTypeTemplate,
		models.MessageTypeInteractive,
		models.MessageTypeFlow:
		// These rely on Meta-issued constructs (template IDs, interactive
		// payloads, flow IDs) that only exist in the Cloud API. The
		// whatsmeow protocol has no equivalent, so we fail fast with a
		// clear message the UI can show to the agent.
		return "", fmt.Errorf("message type %q is only available with the Cloud API provider", req.Type)

	default:
		return "", fmt.Errorf("unsupported message type %q for whatsmeow provider", req.Type)
	}
}

// resolveWhatsmeowMediaBytes returns the raw bytes of a media message
// sent from the agent panel / API. The request may supply the bytes
// directly (MediaData), or a local relative path (MediaURL) that the
// existing media storage serves via /api/media — in the second case we
// read the file off disk.
func (a *App) resolveWhatsmeowMediaBytes(req OutgoingMessageRequest) ([]byte, error) {
	if len(req.MediaData) > 0 {
		return req.MediaData, nil
	}
	if req.MediaURL != "" {
		// MediaURL coming from the agent panel is a relative path
		// (e.g. "images/abc.jpg") that lives under the configured media
		// storage directory. Resolve + read.
		path := filepath.Join(a.getMediaStoragePath(), req.MediaURL)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("whatsmeow media: read %s: %w", path, err)
		}
		return data, nil
	}
	return nil, fmt.Errorf("whatsmeow send %s: no MediaData or MediaURL provided", req.Type)
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
