package models

import (
	"time"

	"github.com/google/uuid"
)

// CRMEventQueue is a persistent queue of CRM events that need to be (re)sent
// to the external CRM. Used as a fallback when:
//
//   - The CRM is offline at the moment a call/message event happens.
//   - The CRM responds with 5xx and we want to retry exponentially.
//   - The HTTP request fails for transport reasons (timeout, DNS, TLS, etc.).
//
// A background worker (internal/integrations/crm.QueueWorker) periodically
// scans this table for rows with status='pending' and next_attempt_at <= now,
// tries to deliver them, and updates status accordingly.
type CRMEventQueue struct {
	BaseModel
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`

	// EventType matches the "event" field of the JSON payload (e.g.
	// "call.ringing", "call.ended", "message.inbound").
	EventType string `gorm:"size:64;not null;index" json:"event_type"`

	// Endpoint is the URL the worker will POST to.
	Endpoint string `gorm:"size:512;not null" json:"endpoint"`

	// Payload is the full JSON body to POST. Stored as text so we can hold
	// large payloads (recording metadata, IVR paths, etc.) without size
	// surprises.
	Payload string `gorm:"type:text;not null" json:"payload"`

	// Signature is the precomputed HMAC SHA256 of timestamp+payload, base64
	// encoded. Captured at enqueue time so retries reuse the original
	// signature (Telnyx-style).
	Signature string `gorm:"size:128;not null" json:"signature"`

	// Timestamp is the Unix epoch second the event was first enqueued. Sent
	// as the X-iReparo-Timestamp header so the CRM can verify the HMAC.
	Timestamp int64 `gorm:"not null" json:"timestamp"`

	// Status: pending | delivered | dead_letter.
	Status string `gorm:"size:20;not null;default:'pending';index" json:"status"`

	// AttemptCount is the number of delivery attempts made so far.
	AttemptCount int `gorm:"not null;default:0" json:"attempt_count"`

	// NextAttemptAt is the wall-clock time the worker should retry this
	// event. NULL means "ready immediately". Set on backoff.
	NextAttemptAt *time.Time `gorm:"index" json:"next_attempt_at,omitempty"`

	// LastError is the last delivery error (HTTP status, message). Helpful
	// for debugging dead-lettered events from the admin UI.
	LastError string `gorm:"type:text" json:"last_error,omitempty"`

	// DeliveredAt is set on the first 2xx response.
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

func (CRMEventQueue) TableName() string {
	return "crm_event_queue"
}
