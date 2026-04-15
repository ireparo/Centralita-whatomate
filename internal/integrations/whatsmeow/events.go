package whatsmeow

import (
	"strings"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// extractIncomingFields decodes a whatsmeow *events.Message into the
// trimmed view the iReparo pipeline expects.
//
// Message types in the WhatsApp Web protocol are encoded as a one-of
// across a dozen proto fields (Conversation for plain text,
// ExtendedTextMessage for text with formatting/mentions/quotes,
// ImageMessage / AudioMessage / etc. for media). We collapse them to
// our flat models.MessageType enum.
func extractIncomingFields(evt *events.Message) *IncomingMessage {
	msg := evt.Message
	info := evt.Info

	out := &IncomingMessage{
		MessageID: info.ID,
		FromJID:   info.Sender,
		FromPhone: jidToPhone(info.Sender),
		PushName:  info.PushName,
		Timestamp: info.Timestamp,
		Raw:       evt,
	}

	// Group detection — whatsmeow sets Info.Chat to the group JID
	// (*@g.us) while Info.Sender is the participant. If the chat and
	// sender differ and the chat ends in @g.us, it's a group message.
	if info.Chat.Server == types.GroupServer {
		out.IsGroup = true
		out.GroupJID = info.Chat
		out.GroupPhone = info.Chat.User // the group ID portion
	}

	switch {
	case msg.GetConversation() != "":
		out.Type = "text"
		out.Content = msg.GetConversation()

	case msg.GetExtendedTextMessage() != nil:
		out.Type = "text"
		out.Content = msg.GetExtendedTextMessage().GetText()

	case msg.GetImageMessage() != nil:
		im := msg.GetImageMessage()
		out.Type = "image"
		out.Content = im.GetCaption()
		out.MediaURL = im.GetURL()
		out.MediaMime = im.GetMimetype()

	case msg.GetAudioMessage() != nil:
		am := msg.GetAudioMessage()
		out.Type = "audio"
		out.MediaURL = am.GetURL()
		out.MediaMime = am.GetMimetype()

	case msg.GetVideoMessage() != nil:
		vm := msg.GetVideoMessage()
		out.Type = "video"
		out.Content = vm.GetCaption()
		out.MediaURL = vm.GetURL()
		out.MediaMime = vm.GetMimetype()

	case msg.GetDocumentMessage() != nil:
		dm := msg.GetDocumentMessage()
		out.Type = "document"
		out.Content = dm.GetCaption()
		out.MediaURL = dm.GetURL()
		out.MediaMime = dm.GetMimetype()

	case msg.GetStickerMessage() != nil:
		sm := msg.GetStickerMessage()
		out.Type = "sticker"
		out.MediaURL = sm.GetURL()
		out.MediaMime = sm.GetMimetype()

	case msg.GetReactionMessage() != nil:
		out.Type = "reaction"
		out.Content = msg.GetReactionMessage().GetText()

	case msg.GetButtonsResponseMessage() != nil:
		out.Type = "interactive"
		out.Content = msg.GetButtonsResponseMessage().GetSelectedDisplayText()

	case msg.GetListResponseMessage() != nil:
		out.Type = "interactive"
		out.Content = msg.GetListResponseMessage().GetTitle()

	default:
		// Unknown / unsupported subtype — keep as "text" with empty
		// body so the webhook pipeline at least logs it and the
		// conversation timeline shows something.
		out.Type = "text"
		out.Content = ""
	}

	return out
}

// jidToPhone converts a whatsmeow JID ("34666123456.0:0@s.whatsapp.net")
// into the E.164-no-plus form used throughout iReparo ("34666123456").
//
// The JID's User field is already the bare phone for user accounts; we
// strip any device suffix ".N" that whatsmeow adds for multi-device
// sessions.
func jidToPhone(jid types.JID) string {
	user := jid.User
	if i := strings.IndexByte(user, '.'); i >= 0 {
		user = user[:i]
	}
	if i := strings.IndexByte(user, ':'); i >= 0 {
		user = user[:i]
	}
	return user
}
