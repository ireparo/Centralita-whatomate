package crm

import "time"

// Event types emitted by iReparo PBX to the external CRM. Keep this list
// in sync with docs/crm-integration-spec.md so the Laravel side knows
// exactly what to expect.
//
// All events share the same outer envelope:
//
//	{
//	  "event":     "<event_type>",
//	  "timestamp": "2026-04-09T18:12:34.567Z",   // RFC3339 UTC
//	  "data":      { ... event-specific fields ... }
//	}
const (
	EventCallRinging      = "call.ringing"
	EventCallAnswered     = "call.answered"
	EventCallEnded        = "call.ended"
	EventCallMissed       = "call.missed"
	EventMessageInbound   = "message.inbound"
	EventMessageOutbound  = "message.outbound"
	EventContactCreated   = "contact.created"
	EventContactUpdated   = "contact.updated"
)

// Envelope is the wrapper sent in the body of every POST /api/pbx/call-event.
type Envelope struct {
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}

// AgentRef is a compact reference to an iReparo agent (User), used in
// payloads where we need to identify who handled a call.
type AgentRef struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
}

// CallRingingData — payload for "call.ringing".
type CallRingingData struct {
	CallID         string `json:"call_id"`         // PBX-side unique id (e.g. WhatsApp wamid or Telnyx call_control_id)
	Direction      string `json:"direction"`       // "incoming" | "outgoing"
	CallerPhone    string `json:"caller_phone"`    // E.164 without +
	CalledPhone    string `json:"called_phone"`    // E.164 without +
	PBXContactID   string `json:"pbx_contact_id"`  // iReparo Contact UUID
	ExternalCRMID  *int64 `json:"external_crm_id"` // CRM customers.id if known
	IVRFlowName    string `json:"ivr_flow_name"`
	Channel        string `json:"channel"` // "whatsapp" | "telnyx_pstn"
}

// CallAnsweredData — payload for "call.answered".
type CallAnsweredData struct {
	CallID        string    `json:"call_id"`
	AnsweredAt    time.Time `json:"answered_at"`
	Agent         *AgentRef `json:"agent,omitempty"`
	ViaTransfer   bool      `json:"via_transfer"`
	Channel       string    `json:"channel"`
	ExternalCRMID *int64    `json:"external_crm_id"`
}

// CallEndedData — payload for "call.ended".
type CallEndedData struct {
	CallID                   string     `json:"call_id"`
	Direction                string     `json:"direction"`
	CallerPhone              string     `json:"caller_phone"`
	CalledPhone              string     `json:"called_phone"`
	PBXContactID             string     `json:"pbx_contact_id"`
	ExternalCRMID            *int64     `json:"external_crm_id"`
	Status                   string     `json:"status"` // "completed" | "missed" | "rejected" | "failed"
	DurationSeconds          int        `json:"duration_seconds"`
	StartedAt                time.Time  `json:"started_at"`
	AnsweredAt               *time.Time `json:"answered_at,omitempty"`
	EndedAt                  time.Time  `json:"ended_at"`
	DisconnectedBy           string     `json:"disconnected_by"`
	Agent                    *AgentRef  `json:"agent,omitempty"`
	IVRPath                  []string   `json:"ivr_path,omitempty"`
	RecordingURL             string     `json:"recording_url,omitempty"`
	RecordingDurationSeconds int        `json:"recording_duration_seconds,omitempty"`
	Channel                  string     `json:"channel"`
}

// CallMissedData — payload for "call.missed". A separate event from
// "call.ended" so the CRM can branch its UI/automation cleanly (e.g.
// "open ticket" only on missed calls, not on completed ones).
type CallMissedData struct {
	CallID                  string `json:"call_id"`
	CallerPhone             string `json:"caller_phone"`
	CalledPhone             string `json:"called_phone"`
	PBXContactID            string `json:"pbx_contact_id"`
	ExternalCRMID           *int64 `json:"external_crm_id"`
	Reason                  string `json:"reason"` // "no_agent_available" | "caller_hung_up" | "busy"
	Channel                 string `json:"channel"`
	WhatsappFallbackSent    bool   `json:"whatsapp_fallback_sent"`
}

// MessageInboundData — payload for "message.inbound".
type MessageInboundData struct {
	MessageID       string `json:"message_id"`
	FromPhone       string `json:"from_phone"`
	PBXContactID    string `json:"pbx_contact_id"`
	ExternalCRMID   *int64 `json:"external_crm_id"`
	Type            string `json:"type"` // "text" | "image" | "audio" | "document" | "template" | "button_reply"
	Content         string `json:"content"`
	MediaURL        string `json:"media_url,omitempty"`
	WhatsAppAccount string `json:"whatsapp_account"`
}

// MessageOutboundData — payload for "message.outbound".
type MessageOutboundData struct {
	MessageID       string             `json:"message_id"`
	ToPhone         string             `json:"to_phone"`
	PBXContactID    string             `json:"pbx_contact_id"`
	ExternalCRMID   *int64             `json:"external_crm_id"`
	Type            string             `json:"type"`
	Content         string             `json:"content"`
	SentBy          *MessageSenderInfo `json:"sent_by,omitempty"`
	WhatsAppAccount string             `json:"whatsapp_account"`
}

// MessageSenderInfo identifies who triggered an outbound message.
type MessageSenderInfo struct {
	Type      string `json:"type"` // "agent" | "chatbot" | "template" | "missed_call_fallback" | "campaign"
	AgentID   string `json:"agent_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
}

// LookupResponse is what the CRM returns from GET /api/pbx/lookup.
//
// found=false means the phone is not in the CRM yet (and the PBX should
// show "Unknown caller" + a "Create customer" button to the agent).
type LookupResponse struct {
	Found           bool      `json:"found"`
	NormalizedPhone string    `json:"normalized_phone"`
	Customer        *Customer `json:"customer,omitempty"`
	CreateURL       string    `json:"create_url,omitempty"`
}

// Customer is the subset of CRM customer fields the PBX displays in the
// agent panel screen-pop. It is intentionally minimal — the agent clicks
// "Open in CRM" if they need the full record.
type Customer struct {
	ID                int64       `json:"id"`
	Name              string      `json:"name"`
	Phone             string      `json:"phone"`
	PhoneAlt          string      `json:"phone_alt,omitempty"`
	Email             string      `json:"email,omitempty"`
	ProfileURL        string      `json:"profile_url"`
	ActiveTicketsCount int        `json:"active_tickets_count"`
	LastTicket        *TicketRef  `json:"last_ticket,omitempty"`
	TotalSpentEUR     float64     `json:"total_spent_eur"`
	FirstSeenAt       *time.Time  `json:"first_seen_at,omitempty"`
	VIP               bool        `json:"vip"`
	NotesSummary      string      `json:"notes_summary,omitempty"`
}

// TicketRef is a compact reference to a CRM ticket included in the lookup
// response so the agent sees the most relevant context immediately.
type TicketRef struct {
	ID             int64     `json:"id"`
	TrackingToken  string    `json:"tracking_token"`
	Status         string    `json:"status"`
	Device         string    `json:"device,omitempty"`
	OpenedAt       time.Time `json:"opened_at"`
	URL            string    `json:"url"`
}
