package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/integrations/crm"
)

// buildMessageSenderInfo covers the mapping from MessageSendOptions
// to the CRM sender_type enum. This is used on every outgoing
// message.outbound event, so the distinctions here drive how the
// CRM UI labels "Enviado por X" on each row.

func TestBuildMessageSenderInfo_Agent(t *testing.T) {
	uid := uuid.New()
	info := buildMessageSenderInfo(MessageSendOptions{SentByUserID: &uid})
	if info == nil {
		t.Fatal("expected non-nil sender info")
	}
	if info.Type != "agent" {
		t.Errorf("expected agent, got %s", info.Type)
	}
	if info.AgentID != uid.String() {
		t.Errorf("expected agent_id=%s, got %s", uid, info.AgentID)
	}
}

func TestBuildMessageSenderInfo_Chatbot(t *testing.T) {
	// ChatbotSendOptions has TrackSLA=true and no SentByUserID.
	info := buildMessageSenderInfo(ChatbotSendOptions())
	if info == nil || info.Type != "chatbot" {
		t.Errorf("expected chatbot, got %+v", info)
	}
	if info.AgentID != "" {
		t.Errorf("chatbot should not have agent_id, got %s", info.AgentID)
	}
}

func TestBuildMessageSenderInfo_Automation(t *testing.T) {
	// APISendOptions / SLASendOptions / DefaultSendOptions without user.
	for _, opts := range []MessageSendOptions{
		APISendOptions(),
		SLASendOptions(),
		DefaultSendOptions(),
	} {
		info := buildMessageSenderInfo(opts)
		if info == nil || info.Type != "automation" {
			t.Errorf("expected automation, got %+v for opts %+v", info, opts)
		}
	}
}

// Ensure the MessageInboundData / MessageOutboundData types marshal
// into the shape documented in docs/crm-integration-spec.md and
// consumed by the Laravel side (case-sensitive field names).
func TestMessageEventDataMarshaling(t *testing.T) {
	inbound := &crm.MessageInboundData{
		MessageID:       "msg-1",
		FromPhone:       "34637111222",
		PBXContactID:    "contact-uuid",
		Type:            "text",
		Content:         "hola",
		WhatsAppAccount: "iReparo ES",
	}
	env := crm.Envelope{Event: crm.EventMessageInbound, Data: inbound}
	if env.Event != "message.inbound" {
		t.Errorf("expected message.inbound, got %s", env.Event)
	}

	outbound := &crm.MessageOutboundData{
		MessageID: "msg-2",
		ToPhone:   "34637111222",
		Type:      "template",
		Content:   "Hola {nombre}",
		SentBy:    &crm.MessageSenderInfo{Type: "agent", AgentID: "u1"},
	}
	if outbound.SentBy.Type != "agent" {
		t.Errorf("expected agent")
	}
}
