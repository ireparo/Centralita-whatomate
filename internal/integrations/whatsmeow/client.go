// Package whatsmeow wraps the go.mau.fi/whatsmeow library so iReparo can
// send and receive WhatsApp messages via the reverse-engineered WhatsApp
// Web protocol, as an alternative to Meta's Cloud API.
//
// When to use this package:
//
//   - Your tenant cannot afford (or qualify for) a Meta Business Cloud API
//     number.
//   - You need features the Cloud API does not expose (group messages,
//     unrestricted media, etc.).
//
// WHAT YOU GIVE UP:
//
//   - Meta can ban the paired number at any moment (ToS violation).
//   - No templates / HSM. The pre-chat 24h window is meaningless here
//     because all inbound messages are session messages.
//   - No official support. Breakage on WhatsApp updates is possible.
//
// The package is designed to coexist with the Cloud API path. Each
// WhatsAppAccount row carries a `provider` column that routes
// message send + receive through either the Cloud API client (in
// pkg/whatsapp) or this wrapper, transparently to the rest of the app.
//
// Architecture:
//
//	+---------------------+        +----------------------+
//	|  SessionManager     |        |  handlers.App        |
//	|  (global, one per   |<------>|  (hooks to persist   |
//	|   process)          | events |   messages, update   |
//	|                     |        |   status, broadcast  |
//	|  accountID -> *Client        |  WS events)          |
//	+----------+----------+        +----------------------+
//	           |
//	           v
//	+---------------------+        +----------------------+
//	|  whatsmeow.Client   |<------>|  WhatsApp servers    |
//	|  (one per account)  |   WS   |  (session via QR)    |
//	+---------------------+        +----------------------+
//
// Sessions persist in the `whatsmeow_*` tables created by whatsmeow's own
// sqlstore. Those run alongside GORM migrations with no conflict because
// they live in separate tables.
package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// Provider is the WhatsAppAccount.Provider value that routes to this
// package. Exposed so callers do not hardcode the string.
const Provider = "whatsmeow"

// ClientState mirrors the high-level lifecycle the session can be in.
// The UI renders these states verbatim.
type ClientState string

const (
	// StateInitialized — client built but not yet connected.
	StateInitialized ClientState = "initialized"
	// StateConnecting — TCP/WS handshake in flight.
	StateConnecting ClientState = "connecting"
	// StateWaitingQR — connection up, waiting for the user to scan a QR.
	StateWaitingQR ClientState = "waiting_qr"
	// StateLoggedIn — paired and ready to send / receive.
	StateLoggedIn ClientState = "logged_in"
	// StateLoggedOut — session invalidated by the user or by WhatsApp.
	StateLoggedOut ClientState = "logged_out"
	// StateError — unrecoverable error, see LastError on the Client.
	StateError ClientState = "error"
)

// EventSink receives the iReparo-facing events the wrapper emits. All
// methods must be safe to call concurrently — the whatsmeow library
// dispatches events on multiple goroutines.
//
// The sink is the seam that lets the integrations package stay free of
// any dependency on internal/handlers. The App implements this interface
// and passes itself to NewSessionManager.
type EventSink interface {
	// OnIncomingMessage is invoked for every text / media message
	// received on the session. evt carries the raw whatsmeow event so
	// the sink can extract whatever fields it needs.
	OnIncomingMessage(ctx context.Context, accountID uuid.UUID, evt *IncomingMessage)

	// OnStateChange reports lifecycle transitions: connecting → waiting_qr
	// → logged_in → logged_out etc.
	OnStateChange(accountID uuid.UUID, from, to ClientState)

	// OnPairSuccess is invoked once the QR has been scanned and the
	// account is now paired. JID is the WhatsApp JID of the device.
	OnPairSuccess(ctx context.Context, accountID uuid.UUID, jid types.JID)

	// OnLoggedOut is invoked when WhatsApp (or the user via the mobile
	// app) tears the session down. Sink typically clears
	// WhatsmeowJID on the account row and flags the account inactive.
	OnLoggedOut(ctx context.Context, accountID uuid.UUID)

	// OnReadReceipt fires when the remote end (the customer) has seen
	// one or more messages we sent. The sink updates Message.Status on
	// those rows to "read" and broadcasts a status_update over WebSocket
	// so the agent panel renders the blue double tick.
	//
	//	deliveryType — "delivered" (single grey tick) or "read" (double
	//	              blue). whatsmeow reports both; the sink decides
	//	              which DB states map.
	OnReadReceipt(ctx context.Context, accountID uuid.UUID, fromPhone string, messageIDs []string, deliveryType string)

	// OnChatPresence fires when the remote end starts or stops typing.
	// The sink broadcasts a WebSocket `typing_indicator` payload to the
	// agent panel so the three-dot bubble renders in real time.
	OnChatPresence(accountID uuid.UUID, fromPhone string, isTyping bool)
}

// IncomingMessage is the trimmed view of a whatsmeow *events.Message we
// hand to the sink. We decode only the subset of fields iReparo
// persists — raw media is downloaded lazily on demand from URL later.
type IncomingMessage struct {
	// MessageID is WhatsApp's stable ID for this message (the wamid
	// equivalent for the Web protocol).
	MessageID string

	// FromJID is the sender's JID (e.g. "34666123456@s.whatsapp.net").
	// For group messages this is the participant JID, NOT the group JID.
	FromJID types.JID

	// FromPhone is the sender's phone number in E.164 without "+".
	// For group messages this is the participant's phone, not the group.
	FromPhone string

	// Type is one of "text", "image", "audio", "video", "document",
	// "sticker", "reaction", "interactive". Mirrors models.MessageType.
	Type string

	// Content is the text body (or the caption for media messages).
	Content string

	// MediaURL is the WhatsApp CDN URL (opaque, requires whatsmeow's
	// Download() to fetch). Only populated for media types.
	MediaURL string

	// MediaMime is the declared mime type for media messages.
	MediaMime string

	// PushName is the display name the sender currently advertises. Used
	// to initialise Contact.ProfileName for new contacts (or SenderName
	// on group messages).
	PushName string

	// Timestamp is when WhatsApp server received the message (not when
	// we got it).
	Timestamp time.Time

	// Group fields (Phase W.4).
	//
	// When IsGroup is true, the chat this message belongs to is the
	// group identified by GroupJID. The message itself came from
	// FromJID / FromPhone (the participant). The sink stores the message
	// on the GROUP Contact with SenderPhone / SenderName populated.
	IsGroup     bool
	GroupJID    types.JID
	GroupPhone  string // "1203.." — the group ID stripped of the @g.us suffix, for Contact.PhoneNumber reuse

	// Raw is the original whatsmeow event. Most callers do not need it;
	// exposed so the sink can down-cast for features the trimmed view
	// does not cover.
	Raw *events.Message
}

// Client is one paired whatsmeow session bound to one WhatsAppAccount.
// Safe for concurrent use.
type Client struct {
	accountID uuid.UUID
	device    *store.Device
	wm        *whatsmeow.Client
	sink      EventSink

	mu        sync.RWMutex
	state     ClientState
	lastError error
	// qrChan carries QR strings (base64 of raw bytes) to subscribers.
	// Refreshed on every call to StartPairing; closed on success or
	// timeout.
	qrChan chan string

	log waLog.Logger
}

// newClient builds a Client from an existing (or fresh) whatsmeow device.
// The public entry point is SessionManager.GetOrCreate — most callers
// should not build a Client directly.
func newClient(accountID uuid.UUID, device *store.Device, sink EventSink, log waLog.Logger) *Client {
	wm := whatsmeow.NewClient(device, log)
	c := &Client{
		accountID: accountID,
		device:    device,
		wm:        wm,
		sink:      sink,
		state:     StateInitialized,
		log:       log,
	}
	wm.AddEventHandler(c.handleEvent)
	return c
}

// State returns the current lifecycle state. Read-safe under concurrency.
func (c *Client) State() ClientState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// LastError returns the last unrecoverable error, or nil if the session
// is healthy.
func (c *Client) LastError() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastError
}

// JID returns the WhatsApp JID of the paired device, or the zero value
// if not yet paired.
func (c *Client) JID() types.JID {
	if c.device == nil || c.device.ID == nil {
		return types.EmptyJID
	}
	return *c.device.ID
}

// IsPaired reports whether the store already has credentials for a
// device. If true, Connect goes straight to logged_in; if false, you
// must run StartPairing first.
func (c *Client) IsPaired() bool {
	return c.device != nil && c.device.ID != nil
}

// Connect opens the session. For a paired client it logs in directly;
// for an unpaired one, subsequent StartPairing presents the QR.
//
// Returns immediately after the handshake starts. Observe State() to
// know when the client reaches StateLoggedIn.
func (c *Client) Connect(ctx context.Context) error {
	c.setState(StateConnecting)
	if err := c.wm.Connect(); err != nil {
		c.setError(fmt.Errorf("whatsmeow connect: %w", err))
		return err
	}
	return nil
}

// StartPairing opens a QR channel that the HTTP/WebSocket handler can
// read to display QR codes to the user. Returns a read-only channel
// that produces successive QR strings until pairing succeeds, the
// context is cancelled, or the process times out (typically 60s per QR,
// total ~3 minutes across refreshes).
//
// Only callable when IsPaired() returns false.
func (c *Client) StartPairing(ctx context.Context) (<-chan string, error) {
	if c.IsPaired() {
		return nil, errors.New("whatsmeow: already paired")
	}
	// whatsmeow requires GetQRChannel BEFORE Connect to capture the
	// initial QR event.
	qrChan, err := c.wm.GetQRChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("whatsmeow qr channel: %w", err)
	}
	out := make(chan string, 4)
	c.mu.Lock()
	c.qrChan = out
	c.mu.Unlock()
	c.setState(StateWaitingQR)

	go func() {
		defer close(out)
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				select {
				case out <- evt.Code:
				default:
					// subscriber is slow — drop older QR, UI only
					// cares about the most recent one.
				}
			case "success":
				// On success, whatsmeow has already stored the device,
				// and the *events.PairSuccess will fire on the event
				// handler. Nothing else to do here.
				return
			case "timeout", "err-client-outdated", "err-scanned-without-multidevice":
				c.setError(fmt.Errorf("whatsmeow qr: %s", evt.Event))
				c.setState(StateError)
				return
			}
		}
	}()

	if err := c.wm.Connect(); err != nil {
		c.setError(fmt.Errorf("whatsmeow connect during pairing: %w", err))
		return nil, err
	}
	return out, nil
}

// Disconnect closes the session gracefully without unpairing. The
// device remains in the store and can be reconnected later.
func (c *Client) Disconnect() {
	if c.wm != nil {
		c.wm.Disconnect()
	}
}

// Logout tears down the session AND invalidates the paired device on
// WhatsApp's servers. After this, IsPaired() returns false and a fresh
// QR is required to use the account again.
func (c *Client) Logout(ctx context.Context) error {
	if c.wm == nil {
		return nil
	}
	if err := c.wm.Logout(ctx); err != nil {
		return fmt.Errorf("whatsmeow logout: %w", err)
	}
	c.setState(StateLoggedOut)
	return nil
}

// resolveRecipientJID builds a JID from a recipient string. Accepts
// either:
//
//	- Plain E.164 no-plus ("34666112233") → individual s.whatsapp.net JID
//	- Bare group ID ("120363111112222333") → group @g.us JID
//	- Full JID string ("...@s.whatsapp.net" / "...@g.us") → parsed as-is
//
// Distinguishing bare groups from individuals: WhatsApp group IDs are
// always longer than any legal phone number (E.164 maxes out at 15
// digits; group IDs are 18+ digits). We use length > 15 as the heuristic
// so the dispatcher does not need a separate IsGroup flag at call
// sites.
func resolveRecipientJID(recipient string) types.JID {
	// Already a full JID?
	if strings.Contains(recipient, "@") {
		if jid, err := types.ParseJID(recipient); err == nil {
			return jid
		}
	}
	if len(recipient) > 15 {
		return types.NewJID(recipient, types.GroupServer)
	}
	return types.NewJID(recipient, types.DefaultUserServer)
}

// SendTextMessage dispatches a text WhatsApp message. Returns the
// server-assigned message ID.
//
// recipient can be an E.164 phone (1:1 chat) OR a WhatsApp group ID
// (for group chats). See resolveRecipientJID for detection rules.
func (c *Client) SendTextMessage(ctx context.Context, recipient, body string) (string, error) {
	if c.State() != StateLoggedIn {
		return "", fmt.Errorf("whatsmeow: session not logged in (state=%s)", c.State())
	}
	jid := resolveRecipientJID(recipient)
	msg := &waProto.Message{
		Conversation: proto(body),
	}
	resp, err := c.wm.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", fmt.Errorf("whatsmeow send: %w", err)
	}
	return resp.ID, nil
}

// MediaPayload is the shape the handler passes to the media-send methods.
// Kept separate from whatsmeow's types so the handlers layer does not
// need to import the upstream proto package.
type MediaPayload struct {
	Data     []byte // raw bytes
	Mime     string // e.g. "image/jpeg"
	Filename string // used for documents; optional for other types
	Caption  string // optional caption for image/video/document
}

// SendImageMessage uploads the bytes to WhatsApp's CDN and sends an
// image message to toPhone. Returns the server-assigned message ID.
func (c *Client) SendImageMessage(ctx context.Context, toPhone string, media MediaPayload) (string, error) {
	if c.State() != StateLoggedIn {
		return "", fmt.Errorf("whatsmeow: session not logged in (state=%s)", c.State())
	}
	if len(media.Data) == 0 {
		return "", fmt.Errorf("whatsmeow: empty image payload")
	}
	up, err := c.wm.Upload(ctx, media.Data, whatsmeow.MediaImage)
	if err != nil {
		return "", fmt.Errorf("whatsmeow upload image: %w", err)
	}
	jid := resolveRecipientJID(toPhone)
	msg := &waProto.Message{
		ImageMessage: &waProto.ImageMessage{
			URL:           proto(up.URL),
			DirectPath:    proto(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto(media.Mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    protoUint64(up.FileLength),
			Caption:       proto(media.Caption),
		},
	}
	resp, err := c.wm.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", fmt.Errorf("whatsmeow send image: %w", err)
	}
	return resp.ID, nil
}

// SendVideoMessage is the video counterpart of SendImageMessage.
func (c *Client) SendVideoMessage(ctx context.Context, toPhone string, media MediaPayload) (string, error) {
	if c.State() != StateLoggedIn {
		return "", fmt.Errorf("whatsmeow: session not logged in (state=%s)", c.State())
	}
	up, err := c.wm.Upload(ctx, media.Data, whatsmeow.MediaVideo)
	if err != nil {
		return "", fmt.Errorf("whatsmeow upload video: %w", err)
	}
	jid := resolveRecipientJID(toPhone)
	msg := &waProto.Message{
		VideoMessage: &waProto.VideoMessage{
			URL:           proto(up.URL),
			DirectPath:    proto(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto(media.Mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    protoUint64(up.FileLength),
			Caption:       proto(media.Caption),
		},
	}
	resp, err := c.wm.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", fmt.Errorf("whatsmeow send video: %w", err)
	}
	return resp.ID, nil
}

// SendAudioMessage sends an audio / voice note. If media.Mime starts with
// "audio/ogg" the message is flagged as a PTT (push-to-talk / voice
// note); otherwise it is a regular audio file.
func (c *Client) SendAudioMessage(ctx context.Context, toPhone string, media MediaPayload) (string, error) {
	if c.State() != StateLoggedIn {
		return "", fmt.Errorf("whatsmeow: session not logged in (state=%s)", c.State())
	}
	up, err := c.wm.Upload(ctx, media.Data, whatsmeow.MediaAudio)
	if err != nil {
		return "", fmt.Errorf("whatsmeow upload audio: %w", err)
	}
	isPTT := len(media.Mime) >= 9 && media.Mime[:9] == "audio/ogg"
	jid := resolveRecipientJID(toPhone)
	msg := &waProto.Message{
		AudioMessage: &waProto.AudioMessage{
			URL:           proto(up.URL),
			DirectPath:    proto(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto(media.Mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    protoUint64(up.FileLength),
			PTT:           protoBool(isPTT),
		},
	}
	resp, err := c.wm.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", fmt.Errorf("whatsmeow send audio: %w", err)
	}
	return resp.ID, nil
}

// SendDocumentMessage sends an arbitrary file as a WhatsApp document.
// media.Filename shows up as the filename hint in the recipient's chat.
func (c *Client) SendDocumentMessage(ctx context.Context, toPhone string, media MediaPayload) (string, error) {
	if c.State() != StateLoggedIn {
		return "", fmt.Errorf("whatsmeow: session not logged in (state=%s)", c.State())
	}
	up, err := c.wm.Upload(ctx, media.Data, whatsmeow.MediaDocument)
	if err != nil {
		return "", fmt.Errorf("whatsmeow upload document: %w", err)
	}
	jid := resolveRecipientJID(toPhone)
	msg := &waProto.Message{
		DocumentMessage: &waProto.DocumentMessage{
			URL:           proto(up.URL),
			DirectPath:    proto(up.DirectPath),
			MediaKey:      up.MediaKey,
			Mimetype:      proto(media.Mime),
			FileEncSHA256: up.FileEncSHA256,
			FileSHA256:    up.FileSHA256,
			FileLength:    protoUint64(up.FileLength),
			FileName:      proto(media.Filename),
			Caption:       proto(media.Caption),
		},
	}
	resp, err := c.wm.SendMessage(ctx, jid, msg)
	if err != nil {
		return "", fmt.Errorf("whatsmeow send document: %w", err)
	}
	return resp.ID, nil
}

// DownloadMedia decrypts and returns the raw bytes of an incoming media
// message. The caller passes the original *events.Message.Raw (from the
// IncomingMessage struct) so whatsmeow can pick the right sub-proto.
//
// Used by the handler on incoming image/audio/video/document events to
// save the bytes to local storage before dispatching to the chat pipeline.
func (c *Client) DownloadMedia(ctx context.Context, msg *waProto.Message) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("whatsmeow download: nil message")
	}
	// whatsmeow.Client.DownloadAny inspects the Message proto and picks
	// whichever media submessage is present (ImageMessage / AudioMessage
	// / VideoMessage / DocumentMessage / StickerMessage).
	return c.wm.DownloadAny(ctx, msg)
}

// SendTypingIndicator tells WhatsApp "this agent is currently typing"
// (or has stopped). The other end renders the classic three-dot bubble.
//
// toPhone is the recipient in E.164 no-plus form. isTyping=false sends
// a "paused" presence which hides the dots immediately; if the typing
// state is never explicitly paused, WhatsApp auto-clears it after ~10s.
func (c *Client) SendTypingIndicator(ctx context.Context, toPhone string, isTyping bool) error {
	if c.State() != StateLoggedIn {
		return fmt.Errorf("whatsmeow: session not logged in (state=%s)", c.State())
	}
	jid := resolveRecipientJID(toPhone)
	presence := types.ChatPresenceComposing
	if !isTyping {
		presence = types.ChatPresencePaused
	}
	// The third arg is the media type being typed (empty = text). The
	// library also accepts "audio" for recording-voice-note indicators,
	// but we do not surface that yet.
	return c.wm.SendChatPresence(jid, presence, types.ChatPresenceMediaText)
}

// MarkRead sends the "read" receipt for one or more incoming messages
// to the sender — the double blue ticks in WhatsApp. Call this from the
// Cloud API auto-read path or from the equivalent whatsmeow flow.
//
//	messageIDs — the server IDs of the messages being marked. Pass the
//	             Messages the agent has now seen in the panel.
//	senderPhone — the JID's user part in E.164 no-plus form.
//
// No-ops (returns nil) when the list is empty.
func (c *Client) MarkRead(ctx context.Context, messageIDs []string, senderPhone string) error {
	if c.State() != StateLoggedIn {
		return fmt.Errorf("whatsmeow: session not logged in (state=%s)", c.State())
	}
	if len(messageIDs) == 0 {
		return nil
	}
	senderJID := types.NewJID(senderPhone, types.DefaultUserServer)
	// whatsmeow's MarkRead takes a MessageIDs slice, a timestamp (when the
	// agent actually saw the message — time.Now is fine for immediate
	// marking), the chat JID, and the sender JID (same as chat for 1:1
	// conversations; differs in groups which W.1 does not cover).
	return c.wm.MarkRead(messageIDs, time.Now(), senderJID, senderJID)
}

// protoUint64 / protoBool mirror `proto` for uint64 / bool fields on the
// whatsmeow proto. Used by the media send methods.
func protoUint64(v uint64) *uint64 { return &v }
func protoBool(v bool) *bool       { return &v }

// --- Internal event handling ---------------------------------------------

func (c *Client) setState(s ClientState) {
	c.mu.Lock()
	prev := c.state
	c.state = s
	c.mu.Unlock()
	if c.sink != nil {
		c.sink.OnStateChange(c.accountID, prev, s)
	}
}

func (c *Client) setError(err error) {
	c.mu.Lock()
	c.lastError = err
	c.mu.Unlock()
}

// handleEvent is the sink for raw whatsmeow events. We translate the
// subset we care about into calls on the EventSink interface.
func (c *Client) handleEvent(rawEvt interface{}) {
	ctx := context.Background()
	switch evt := rawEvt.(type) {
	case *events.Connected:
		// If the device already has an ID, we're good. The paired-flow
		// success event PairSuccess fires separately before this.
		if c.IsPaired() {
			c.setState(StateLoggedIn)
		}
	case *events.PairSuccess:
		c.setState(StateLoggedIn)
		if c.sink != nil {
			c.sink.OnPairSuccess(ctx, c.accountID, evt.ID)
		}
	case *events.LoggedOut:
		c.setState(StateLoggedOut)
		if c.sink != nil {
			c.sink.OnLoggedOut(ctx, c.accountID)
		}
	case *events.Disconnected:
		// Transient — library retries automatically.
	case *events.Message:
		c.handleIncomingMessage(ctx, evt)
	case *events.Receipt:
		c.handleReceipt(ctx, evt)
	case *events.ChatPresence:
		c.handleChatPresence(evt)
	}
}

// handleReceipt bridges a whatsmeow Receipt event (delivery + read)
// into the sink. whatsmeow batches receipts: one event can report
// multiple message IDs, all from the same sender.
func (c *Client) handleReceipt(ctx context.Context, evt *events.Receipt) {
	if c.sink == nil || evt == nil || len(evt.MessageIDs) == 0 {
		return
	}
	// Whatsmeow's Receipt has a Type (ReceiptTypeDelivered /
	// ReceiptTypeRead / ReceiptTypeReadSelf). We only care about the
	// first two for agent-panel updates.
	dt := ""
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		dt = "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		dt = "read"
	default:
		return
	}
	// evt.MessageSource embeds Sender (types.JID) — use the user part.
	c.sink.OnReadReceipt(ctx, c.accountID, jidToPhone(evt.Sender), asStrings(evt.MessageIDs), dt)
}

// handleChatPresence routes typing start/stop events from the remote
// end to the sink.
func (c *Client) handleChatPresence(evt *events.ChatPresence) {
	if c.sink == nil || evt == nil {
		return
	}
	isTyping := evt.State == types.ChatPresenceComposing
	c.sink.OnChatPresence(c.accountID, jidToPhone(evt.Sender), isTyping)
}

// asStrings converts a slice of types.MessageID (an alias for string
// in most whatsmeow versions) into []string. Kept as a helper so the
// conversion stays in one place should whatsmeow change the type.
func asStrings(ids []types.MessageID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}

// handleIncomingMessage decodes a whatsmeow Message event into the
// trimmed IncomingMessage struct and forwards it to the sink.
func (c *Client) handleIncomingMessage(ctx context.Context, evt *events.Message) {
	if c.sink == nil || evt == nil || evt.Message == nil {
		return
	}
	msg := extractIncomingFields(evt)
	c.sink.OnIncomingMessage(ctx, c.accountID, msg)
}

// proto returns a pointer to the supplied string. whatsmeow protos use
// pointers everywhere; this helper avoids inline `s := ""; &s` noise.
func proto(s string) *string { return &s }
