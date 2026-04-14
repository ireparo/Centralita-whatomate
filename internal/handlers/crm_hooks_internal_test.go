package handlers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
)

// Internal-package unit tests for the CRM message event builders. These
// cover pure logic (no DB) so they can run without the full test harness.

func TestBuildInboundMessageData(t *testing.T) {
	orgID := uuid.New()
	contactID := uuid.New()
	ext := int64(42)

	msg := &models.Message{
		BaseModel:         models.BaseModel{ID: uuid.New()},
		WhatsAppMessageID: "wamid.abc123",
		MessageType:       models.MessageTypeText,
		Content:           "Hola!",
		MediaURL:          "",
	}
	contact := &models.Contact{
		BaseModel:     models.BaseModel{ID: contactID},
		PhoneNumber:   "+34 666 11 22 33",
		ExternalCRMID: &ext,
	}
	account := &models.WhatsAppAccount{
		BaseModel:      models.BaseModel{ID: uuid.New()},
		OrganizationID: orgID,
		Name:           "iReparo Main",
	}

	data := buildInboundMessageData(msg, contact, account)
	if data == nil {
		t.Fatal("expected non-nil data")
	}
	if data.MessageID != "wamid.abc123" {
		t.Errorf("MessageID: got %q want %q", data.MessageID, "wamid.abc123")
	}
	if data.FromPhone != "34666112233" {
		t.Errorf("FromPhone not normalized: got %q want %q", data.FromPhone, "34666112233")
	}
	if data.PBXContactID != contactID.String() {
		t.Errorf("PBXContactID: got %q want %q", data.PBXContactID, contactID.String())
	}
	if data.ExternalCRMID == nil || *data.ExternalCRMID != 42 {
		t.Errorf("ExternalCRMID: got %v want 42", data.ExternalCRMID)
	}
	if data.Type != "text" {
		t.Errorf("Type: got %q want %q", data.Type, "text")
	}
	if data.Content != "Hola!" {
		t.Errorf("Content: got %q want %q", data.Content, "Hola!")
	}
	if data.WhatsAppAccount != "iReparo Main" {
		t.Errorf("WhatsAppAccount: got %q want %q", data.WhatsAppAccount, "iReparo Main")
	}
}

func TestBuildInboundMessageData_NilInputs(t *testing.T) {
	cases := []struct {
		name    string
		msg     *models.Message
		contact *models.Contact
		account *models.WhatsAppAccount
	}{
		{"nil msg", nil, &models.Contact{}, &models.WhatsAppAccount{}},
		{"nil contact", &models.Message{}, nil, &models.WhatsAppAccount{}},
		{"nil account", &models.Message{}, &models.Contact{}, nil},
		{"all nil", nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildInboundMessageData(c.msg, c.contact, c.account); got != nil {
				t.Errorf("expected nil, got %+v", got)
			}
		})
	}
}

func TestResolveMessageSender_NoAgent(t *testing.T) {
	// We can exercise the non-DB branches (agent=nil) with only the
	// message's type — the DB is never consulted in this path.
	app := &App{} // no DB needed for these cases

	cases := []struct {
		name    string
		msgType models.MessageType
		wantTy  string
	}{
		{"template", models.MessageTypeTemplate, "template"},
		{"interactive (chatbot)", models.MessageTypeInteractive, "chatbot"},
		{"flow (chatbot)", models.MessageTypeFlow, "chatbot"},
		{"text (campaign fallback)", models.MessageTypeText, "campaign"},
		{"image (campaign fallback)", models.MessageTypeImage, "campaign"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := &models.Message{MessageType: c.msgType}
			info := app.resolveMessageSender(msg)
			if info == nil {
				t.Fatal("expected info, got nil")
			}
			if info.Type != c.wantTy {
				t.Errorf("Type: got %q want %q", info.Type, c.wantTy)
			}
			if info.AgentID != "" {
				t.Errorf("AgentID should be empty, got %q", info.AgentID)
			}
		})
	}
}

func TestResolveMessageSender_NilMessage(t *testing.T) {
	app := &App{}
	if got := app.resolveMessageSender(nil); got != nil {
		t.Errorf("expected nil for nil message, got %+v", got)
	}
}

func TestExternalCRMIDOrNil(t *testing.T) {
	if got := externalCRMIDOrNil(nil); got != nil {
		t.Errorf("nil contact should return nil, got %v", got)
	}

	var none *models.Contact = &models.Contact{}
	if got := externalCRMIDOrNil(none); got != nil {
		t.Errorf("contact without external id should return nil, got %v", got)
	}

	id := int64(123)
	with := &models.Contact{ExternalCRMID: &id}
	got := externalCRMIDOrNil(with)
	if got == nil || *got != 123 {
		t.Errorf("expected 123, got %v", got)
	}
}
