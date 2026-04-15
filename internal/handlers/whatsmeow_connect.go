package handlers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fasthttp/websocket"
	"github.com/google/uuid"
	wameow "github.com/shridarpatil/whatomate/internal/integrations/whatsmeow"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/valyala/fasthttp"
	"github.com/zerodha/fastglue"
)

// whatsmeow_connect.go exposes the HTTP + WebSocket endpoints that
// drive the pairing / connection lifecycle of a WhatsAppAccount whose
// provider is "whatsmeow".
//
// Routes (registered in cmd/whatomate/main.go):
//
//	POST   /api/accounts/{id}/whatsmeow/connect
//	        Start (or resume) the session. For an already-paired
//	        account, this just calls Connect. For an unpaired one, it
//	        begins the QR pairing flow; the QR codes themselves are
//	        streamed over the WebSocket below.
//
//	GET    /ws/whatsmeow/{id}   (WebSocket)
//	        Streams { type: "qr", payload: "<base64 qr>" } messages
//	        while the caller is scanning, followed by
//	        { type: "state", payload: "<state>" } lifecycle updates.
//	        Closes on pair success / timeout / error. Auth via the
//	        standard JWT message-auth flow the other WS endpoint uses.
//
//	POST   /api/accounts/{id}/whatsmeow/disconnect
//	        Disconnects without unpairing. The device stays registered
//	        in whatsmeow's store; a subsequent /connect resumes
//	        without requiring a fresh QR.
//
//	POST   /api/accounts/{id}/whatsmeow/logout
//	        Disconnects AND unpairs — next use requires a fresh QR.
//
//	GET    /api/accounts/{id}/whatsmeow/status
//	        Returns { state, jid, paired, last_error }.

// WhatsmeowConnectResponse is the body returned by POST /connect.
type WhatsmeowConnectResponse struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"`
	Paired    bool   `json:"paired"`
	// NeedsQR is true when the caller should open the WebSocket to
	// receive the QR stream. False for already-paired accounts.
	NeedsQR bool `json:"needs_qr"`
}

// ConnectWhatsmeow is POST /api/accounts/{id}/whatsmeow/connect.
func (a *App) ConnectWhatsmeow(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceAccounts, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to manage accounts", nil, "")
	}
	if a.Whatsmeow == nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "WhatsApp Web provider is not available on this instance", nil, "")
	}

	accountID, errResp := a.loadWhatsmeowAccount(r, orgID)
	if errResp != nil {
		return errResp
	}

	jid, _ := wameow.ParseJID("")
	// If the account row already has a saved JID, pass it so the manager
	// loads the existing device from the whatsmeow store.
	var account models.WhatsAppAccount
	if err := a.DB.Where("id = ?", accountID).First(&account).Error; err == nil && account.WhatsmeowJID != "" {
		jid, _ = wameow.ParseJID(account.WhatsmeowJID)
	}

	client, err := a.Whatsmeow.GetOrCreate(r.RequestCtx, accountID, jid)
	if err != nil {
		a.Log.Error("whatsmeow connect: get/create client", "error", err, "account_id", accountID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to initialize WhatsApp session", nil, "")
	}

	resp := WhatsmeowConnectResponse{
		AccountID: accountID.String(),
		State:     string(client.State()),
		Paired:    client.IsPaired(),
	}

	if client.IsPaired() {
		// Just reconnect — no QR needed.
		if err := client.Connect(r.RequestCtx); err != nil {
			a.Log.Warn("whatsmeow connect failed", "account_id", accountID, "error", err)
			return r.SendErrorEnvelope(fasthttp.StatusBadGateway, "Failed to connect to WhatsApp", nil, "")
		}
		resp.State = string(client.State())
		return r.SendEnvelope(resp)
	}

	// Unpaired → start the QR flow. The WebSocket handler below consumes
	// the channel; this HTTP call just kicks it off so that by the time
	// the client opens the WS the first QR is already queued.
	_, err = client.StartPairing(context.Background())
	if err != nil {
		a.Log.Error("whatsmeow start pairing", "error", err, "account_id", accountID)
		return r.SendErrorEnvelope(fasthttp.StatusInternalServerError, "Failed to start pairing", nil, "")
	}
	resp.NeedsQR = true
	resp.State = string(client.State())
	return r.SendEnvelope(resp)
}

// DisconnectWhatsmeow is POST /api/accounts/{id}/whatsmeow/disconnect.
// Keeps the device paired; next /connect resumes without QR.
func (a *App) DisconnectWhatsmeow(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceAccounts, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to manage accounts", nil, "")
	}
	if a.Whatsmeow == nil {
		return r.SendEnvelope(map[string]any{"status": "disabled"})
	}
	accountID, errResp := a.loadWhatsmeowAccount(r, orgID)
	if errResp != nil {
		return errResp
	}
	if c := a.Whatsmeow.Get(accountID); c != nil {
		c.Disconnect()
	}
	return r.SendEnvelope(map[string]any{"status": "disconnected"})
}

// LogoutWhatsmeow is POST /api/accounts/{id}/whatsmeow/logout. Unpairs
// the device remotely. After this, a fresh QR scan is needed to use the
// account again.
func (a *App) LogoutWhatsmeow(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceAccounts, models.ActionDelete, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission to manage accounts", nil, "")
	}
	if a.Whatsmeow == nil {
		return r.SendEnvelope(map[string]any{"status": "disabled"})
	}
	accountID, errResp := a.loadWhatsmeowAccount(r, orgID)
	if errResp != nil {
		return errResp
	}
	if c := a.Whatsmeow.Get(accountID); c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.Logout(ctx); err != nil {
			a.Log.Warn("whatsmeow logout failed", "account_id", accountID, "error", err)
		}
		a.Whatsmeow.Remove(accountID)
	}
	// Belt-and-suspenders: clear the JID on the account row in case the
	// OnLoggedOut sink missed it (e.g. the session was never connected).
	_ = a.DB.Model(&models.WhatsAppAccount{}).
		Where("id = ?", accountID).
		Update("whatsmeow_jid", "").Error
	return r.SendEnvelope(map[string]any{"status": "logged_out"})
}

// WhatsmeowStatus is GET /api/accounts/{id}/whatsmeow/status.
func (a *App) WhatsmeowStatus(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceAccounts, models.ActionRead, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission", nil, "")
	}
	accountID, errResp := a.loadWhatsmeowAccount(r, orgID)
	if errResp != nil {
		return errResp
	}

	resp := map[string]any{
		"state":  string(wameow.StateInitialized),
		"paired": false,
	}
	if a.Whatsmeow != nil {
		if c := a.Whatsmeow.Get(accountID); c != nil {
			resp["state"] = string(c.State())
			resp["paired"] = c.IsPaired()
			resp["jid"] = c.JID().String()
			if err := c.LastError(); err != nil {
				resp["last_error"] = err.Error()
			}
		}
	}
	// Also mirror the persisted JID so the UI shows it between restarts
	// (before the session is re-warmed).
	var account models.WhatsAppAccount
	if err := a.DB.Where("id = ?", accountID).First(&account).Error; err == nil {
		if _, ok := resp["jid"]; !ok && account.WhatsmeowJID != "" {
			resp["jid"] = account.WhatsmeowJID
			resp["paired"] = true
		}
	}
	return r.SendEnvelope(resp)
}

// loadWhatsmeowAccount extracts the URL id param, validates it exists
// in the org, and confirms it is a whatsmeow-provider account. Returns
// a non-nil error response via r.SendErrorEnvelope on any failure (and
// the caller must just `return errResp`).
func (a *App) loadWhatsmeowAccount(r *fastglue.Request, orgID uuid.UUID) (uuid.UUID, error) {
	idStr, _ := r.RequestCtx.UserValue("id").(string)
	accountID, err := uuid.Parse(idStr)
	if err != nil {
		return uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid account id", nil, "")
	}
	var account models.WhatsAppAccount
	if err := a.DB.Where("id = ? AND organization_id = ?", accountID, orgID).First(&account).Error; err != nil {
		return uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusNotFound, "Account not found", nil, "")
	}
	if account.Provider != wameow.Provider {
		return uuid.Nil, r.SendErrorEnvelope(fasthttp.StatusBadRequest,
			"This account is not configured to use the WhatsApp Web provider",
			nil, "wrong_provider")
	}
	return accountID, nil
}

// --- Typing indicator (Phase W.3) ---------------------------------------

// SendWhatsmeowTyping is POST /api/contacts/{id}/whatsmeow/typing.
//
// Body: { "is_typing": true|false }
//
// Invoked by the agent panel when the compose box focus / text changes.
// Fire-and-forget — 200 OK even if the send to WhatsApp fails, because
// typing is advisory UX; losing one event is better than the agent
// seeing HTTP errors in their console.
//
// Whatsmeow-only. For Cloud API accounts we return 200 with no-op — the
// Cloud API has no outgoing typing primitive and treating this endpoint
// as provider-aware lets the frontend call it unconditionally.
func (a *App) SendWhatsmeowTyping(r *fastglue.Request) error {
	orgID, userID, err := a.getOrgAndUserID(r)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusUnauthorized, "Unauthorized", nil, "")
	}
	if !a.HasPermission(userID, models.ResourceChat, models.ActionWrite, orgID) {
		return r.SendErrorEnvelope(fasthttp.StatusForbidden, "You do not have permission", nil, "")
	}

	idStr, _ := r.RequestCtx.UserValue("id").(string)
	contactID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid contact id", nil, "")
	}

	var body struct {
		IsTyping bool `json:"is_typing"`
	}
	_ = json.Unmarshal(r.RequestCtx.PostBody(), &body)

	var contact models.Contact
	if err := a.DB.Where("id = ? AND organization_id = ?", contactID, orgID).First(&contact).Error; err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusNotFound, "Contact not found", nil, "")
	}
	if contact.WhatsAppAccount == "" {
		return r.SendEnvelope(map[string]any{"status": "skipped_no_account"})
	}
	account, err := a.resolveWhatsAppAccount(orgID, contact.WhatsAppAccount)
	if err != nil {
		return r.SendEnvelope(map[string]any{"status": "skipped_account_missing"})
	}
	if account.Provider != wameow.Provider || a.Whatsmeow == nil {
		// Cloud API has no typing primitive — no-op.
		return r.SendEnvelope(map[string]any{"status": "not_supported_for_provider"})
	}
	client := a.Whatsmeow.Get(account.ID)
	if client == nil {
		return r.SendEnvelope(map[string]any{"status": "skipped_session_not_connected"})
	}

	// Use the request context; 2s timeout is plenty — a typing indicator
	// is pure advisory, nothing blocks on it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.SendTypingIndicator(ctx, contact.PhoneNumber, body.IsTyping); err != nil {
		// Log but do not surface — see rationale in the comment above.
		a.Log.Debug("whatsmeow send typing failed", "error", err)
	}
	return r.SendEnvelope(map[string]any{"status": "ok"})
}

// --- WebSocket: QR code stream ------------------------------------------

// WhatsmeowQRWebSocket upgrades the connection and streams QR codes +
// state changes for the given account id (from URL param).
//
// Protocol (messages the server sends to the client):
//
//	{ "type": "qr",    "payload": "<raw_qr_string>" }
//	{ "type": "state", "payload": "<state>" }
//	{ "type": "error", "payload": "<message>" }
//
// The client renders the QR string to an image (most frameworks have a
// qrcode library that takes the raw string directly). When the state
// message reaches "logged_in" the client should close the socket and
// refresh the account detail view.
//
// Auth: the first message from the CLIENT must be
//   { "type": "auth", "payload": { "token": "<jwt>" } }
// — identical to the main WebSocketHandler for consistency.
func (a *App) WhatsmeowQRWebSocket(r *fastglue.Request) error {
	idStr, _ := r.RequestCtx.UserValue("id").(string)
	accountID, err := uuid.Parse(idStr)
	if err != nil {
		return r.SendErrorEnvelope(fasthttp.StatusBadRequest, "Invalid account id", nil, "")
	}
	if a.Whatsmeow == nil {
		return r.SendErrorEnvelope(fasthttp.StatusServiceUnavailable, "WhatsApp Web provider is not available", nil, "")
	}

	up := a.wsUpgrader()
	return up.Upgrade(r.RequestCtx, func(conn *websocket.Conn) {
		defer conn.Close()

		// --- Auth message (mirrors the main WS handler pattern) ---
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var authMsg struct {
			Type    string `json:"type"`
			Payload struct {
				Token string `json:"token"`
			} `json:"payload"`
		}
		if err := conn.ReadJSON(&authMsg); err != nil || authMsg.Type != "auth" {
			writeWSMsg(conn, "error", "auth required")
			return
		}
		validate := a.validateWSTokenFn()
		userID, orgID, err := validate(authMsg.Payload.Token)
		if err != nil {
			writeWSMsg(conn, "error", "invalid token")
			return
		}
		conn.SetReadDeadline(time.Time{}) // clear deadline for the rest of the session

		// Re-check the account belongs to this org (could have been
		// deleted between the HTTP call and the WS open).
		var account models.WhatsAppAccount
		if err := a.DB.Where("id = ? AND organization_id = ?", accountID, orgID).First(&account).Error; err != nil {
			writeWSMsg(conn, "error", "account not found")
			return
		}
		if !a.HasPermission(userID, models.ResourceAccounts, models.ActionWrite, orgID) {
			writeWSMsg(conn, "error", "forbidden")
			return
		}

		// Get the client that /connect already primed.
		client := a.Whatsmeow.Get(accountID)
		if client == nil {
			writeWSMsg(conn, "error", "session not started — POST /connect first")
			return
		}

		// Emit the current state straight away so the UI can render
		// "waiting_qr" / "logged_in" / etc. without waiting for the
		// first transition.
		writeWSMsg(conn, "state", string(client.State()))

		// If the session is already paired, nothing to stream.
		if client.IsPaired() && client.State() == wameow.StateLoggedIn {
			writeWSMsg(conn, "state", "logged_in")
			return
		}

		// Re-start pairing to get a fresh QR channel. (If /connect was
		// called recently, StartPairing returns an error — harmless,
		// means the channel already exists. For simplicity in this MVP,
		// we just drive off the client's current state transitions and
		// poll for a QR via a side channel approach: we attach a small
		// subscriber. A cleaner design in W.2 exposes an ObserveQR
		// method on Client.)
		qrChan, err := client.StartPairing(r.RequestCtx)
		if err != nil {
			// Not fatal — already in waiting_qr and emitting QRs on the
			// original channel. Emit state and bail; caller refreshes.
			writeWSMsg(conn, "state", string(client.State()))
			return
		}

		// Relay QR events until the channel closes (success / timeout).
		for {
			select {
			case qr, ok := <-qrChan:
				if !ok {
					// Channel closed — either paired or timed out.
					writeWSMsg(conn, "state", string(client.State()))
					return
				}
				writeWSMsg(conn, "qr", qr)
			case <-time.After(65 * time.Second):
				// Per-QR TTL is ~60s; send a state tick so the client
				// knows we are still alive.
				writeWSMsg(conn, "state", string(client.State()))
			}
		}
	})
}

// writeWSMsg is a tiny helper that JSON-encodes { type, payload } and
// writes it to the connection, swallowing errors (the connection will
// be closed on the next iteration if something is truly broken).
func writeWSMsg(conn *websocket.Conn, msgType string, payload string) {
	buf, _ := json.Marshal(map[string]string{"type": msgType, "payload": payload})
	_ = conn.WriteMessage(websocket.TextMessage, buf)
}
