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
	"github.com/shridarpatil/whatomate/internal/websocket"
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

	// Phase W.7: reactions are not real messages — they mutate the
	// Metadata.reactions array on the TARGET message instead of creating
	// a new row. Reuse the exact same handler the Cloud API webhook
	// uses so both provider paths converge on identical state.
	if evt.Type == "reaction" {
		if evt.ReactionTargetID == "" {
			a.Log.Warn("whatsmeow: reaction without target message ID",
				"account_id", accountID, "from", evt.FromPhone)
			return
		}
		a.handleIncomingReaction(&account, evt.FromPhone, evt.ReactionTargetID, evt.Content, evt.PushName)
		return
	}

	// Phase W.4: group messages land on a group Contact (keyed by the
	// group JID) with SenderPhone / SenderName populated from the
	// participant so the UI can render "Alice: Hola!".
	//
	// For 1:1 messages this stays exactly as before — the chat contact
	// is the person who sent the message.
	var contact *models.Contact
	var senderName string
	if evt.IsGroup {
		contact, err = a.resolveOrCreateGroupContact(&account, evt.GroupPhone, evt.GroupJID.String(), evt.PushName)
		if err != nil {
			a.Log.Error("whatsmeow: resolve group contact failed", "group", evt.GroupPhone, "error", err)
			return
		}
		senderName = evt.PushName
	} else {
		contact, err = a.resolveOrCreateContact(&account, evt.FromPhone, evt.PushName)
		if err != nil {
			a.Log.Error("whatsmeow: resolve contact failed", "phone", evt.FromPhone, "error", err)
			return
		}
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

	// For group messages, back-fill SenderPhone + SenderName on the row
	// that saveIncomingMessage just created. Doing it as a follow-up
	// UPDATE keeps the existing helper shape untouched (it is shared
	// with the Cloud API path, which never has groups).
	if evt.IsGroup {
		_ = a.DB.Model(&models.Message{}).
			Where("organization_id = ? AND whats_app_message_id = ?", account.OrganizationID, evt.MessageID).
			Updates(map[string]any{
				"sender_phone": evt.FromPhone,
				"sender_name":  senderName,
			}).Error
	}
}

// resolveOrCreateGroupContact upserts the Contact row that represents a
// WhatsApp group. We key on (org_id, is_group, group_jid) so that group
// renames do not create duplicates.
func (a *App) resolveOrCreateGroupContact(account *models.WhatsAppAccount, groupPhone, groupJID, pushName string) (*models.Contact, error) {
	var contact models.Contact
	err := a.DB.
		Where("organization_id = ? AND is_group = ? AND group_jid = ?", account.OrganizationID, true, groupJID).
		First(&contact).Error
	if err == nil {
		return &contact, nil
	}
	// Fresh group row. We reuse PhoneNumber for the group ID so the rest
	// of the messaging pipeline (which keys on phone_number) works
	// without changes. Group name starts as the push name of the first
	// sender we see — a Phase W.5 enhancement would call GetGroupInfo
	// on pairing to fetch the real subject, but that requires an extra
	// whatsmeow call that can be slow.
	subject := pushName
	if subject == "" {
		subject = "Group " + groupPhone
	}
	contact = models.Contact{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: account.OrganizationID,
		PhoneNumber:    groupPhone,
		ProfileName:    subject,
		IsGroup:        true,
		GroupJID:       groupJID,
		GroupSubject:   subject,
	}
	if err := a.DB.Create(&contact).Error; err != nil {
		return nil, err
	}
	return &contact, nil
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

// OnReadReceipt updates Message.Status for every message ID in the
// batch + broadcasts a status_update WebSocket event so the agent
// panel renders the tick color immediately. Mirrors what the Cloud
// API webhook does when Meta reports a status=read.
func (a *App) OnReadReceipt(ctx context.Context, accountID uuid.UUID, fromPhone string, messageIDs []string, deliveryType string) {
	if len(messageIDs) == 0 {
		return
	}
	var status models.MessageStatus
	switch deliveryType {
	case "delivered":
		status = models.MessageStatusDelivered
	case "read":
		status = models.MessageStatusRead
	default:
		return
	}

	// Bulk update all messages in this receipt batch. The orgID scope is
	// implicit because the WAMID is globally unique — but we still join
	// via the account row to keep the query tenant-safe.
	var account models.WhatsAppAccount
	if err := a.DB.Where("id = ?", accountID).First(&account).Error; err != nil {
		return
	}
	if err := a.DB.Model(&models.Message{}).
		Where("organization_id = ? AND whats_app_message_id IN ?", account.OrganizationID, messageIDs).
		Update("status", status).Error; err != nil {
		a.Log.Warn("whatsmeow: update message status failed",
			"account_id", accountID, "error", err)
		return
	}

	// Broadcast one status_update per message so the agent panel
	// updates the ticks without a refresh. Mirrors the Cloud API path.
	if a.WSHub == nil {
		return
	}
	for _, id := range messageIDs {
		a.WSHub.BroadcastToOrg(account.OrganizationID, websocket.WSMessage{
			Type: websocket.TypeStatusUpdate,
			Payload: map[string]any{
				"wamid":  id,
				"status": status,
			},
		})
	}
}

// OnChatPresence broadcasts a typing_indicator payload to the agent
// panel. The frontend renders (or hides) the three-dot bubble in the
// chat header. The event is ephemeral — we do not persist it.
func (a *App) OnChatPresence(accountID uuid.UUID, fromPhone string, isTyping bool) {
	if a.WSHub == nil {
		return
	}
	// Resolve org + contact so the frontend can filter the WS message
	// to the chat it is currently showing.
	var account models.WhatsAppAccount
	if err := a.DB.Where("id = ?", accountID).First(&account).Error; err != nil {
		return
	}
	var contact models.Contact
	if err := a.DB.
		Where("organization_id = ? AND phone_number = ?", account.OrganizationID, fromPhone).
		First(&contact).Error; err != nil {
		return
	}
	a.WSHub.BroadcastToOrg(account.OrganizationID, websocket.WSMessage{
		Type: websocket.TypeTypingIndicator,
		Payload: map[string]any{
			"contact_id": contact.ID.String(),
			"account_id": accountID.String(),
			"is_typing":  isTyping,
		},
	})
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
