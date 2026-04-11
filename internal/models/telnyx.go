package models

import (
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/crypto"
)

// TelnyxConnection holds the credentials and configuration for the Telnyx
// account associated with an organization. One organization can have at most
// one Telnyx connection (enforced by uniqueIndex on organization_id).
//
// Sensitive fields (APIKey, PublicKey) are stored encrypted at rest using the
// app encryption key, and decrypted on the fly when the integration code
// needs to use them.
type TelnyxConnection struct {
	BaseModel
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;uniqueIndex" json:"organization_id"`

	// Friendly label shown in the UI ("Telnyx producción", "Cuenta sandbox").
	Label string `gorm:"size:100;not null" json:"label"`

	// APIKey is the Telnyx API v2 key (the one starting with "KEY...").
	// Encrypted at rest.
	APIKey string `gorm:"type:text;not null" json:"-"`

	// PublicKey is the base64-encoded Ed25519 public key Telnyx uses to sign
	// outgoing webhooks. We need it to verify incoming webhook payloads.
	// Encrypted at rest because it identifies the account.
	PublicKey string `gorm:"type:text" json:"-"`

	// CallControlAppID is the Telnyx Call Control Application ID this
	// connection routes calls through. The webhook URL configured in that
	// app must point to https://<host>/api/webhook/telnyx
	CallControlAppID string `gorm:"size:100;not null" json:"call_control_app_id"`

	// OutboundProfileID is the Telnyx Outbound Voice Profile used for
	// outgoing calls. Determines pricing tier and dial-out permissions.
	OutboundProfileID string `gorm:"size:100" json:"outbound_profile_id"`

	// Status: active, suspended, error.
	Status string `gorm:"size:20;not null;default:'active'" json:"status"`

	// LastVerifiedAt records the last time we successfully called the Telnyx
	// API with these credentials, useful to detect rotated keys.
	LastVerifiedAt *time.Time `json:"last_verified_at,omitempty"`

	CreatedByID *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID *uuid.UUID `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	// Relations
	Organization *Organization `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	CreatedBy    *User         `gorm:"foreignKey:CreatedByID" json:"created_by,omitempty"`
	UpdatedBy    *User         `gorm:"foreignKey:UpdatedByID" json:"updated_by,omitempty"`
	Numbers      []TelnyxNumber `gorm:"foreignKey:ConnectionID" json:"numbers,omitempty"`
}

func (TelnyxConnection) TableName() string {
	return "telnyx_connections"
}

// DecryptSecrets decrypts the encrypted credentials in place using the
// supplied app encryption key.
func (c *TelnyxConnection) DecryptSecrets(encryptionKey string) {
	crypto.DecryptFields(encryptionKey, &c.APIKey, &c.PublicKey)
}

// TelnyxNumber represents a single phone number (DDI) the organization owns
// in Telnyx and routes through iReparo. Multiple numbers per connection are
// allowed (e.g., a Spanish geographic number + a toll-free 900 + a mobile
// virtual number, all under the same Telnyx account).
type TelnyxNumber struct {
	BaseModel
	OrganizationID uuid.UUID `gorm:"type:uuid;not null;index" json:"organization_id"`
	ConnectionID   uuid.UUID `gorm:"type:uuid;not null;index" json:"connection_id"`

	// PhoneNumber in E.164 without the leading "+".
	// Example: "34873940702" for "+34 873 94 07 02".
	PhoneNumber string `gorm:"size:20;not null;uniqueIndex" json:"phone_number"`

	// Label shown in the UI ("Recepción Barcelona", "Soporte técnico", etc.)
	Label string `gorm:"size:100" json:"label"`

	// Country code in ISO 3166-1 alpha-2. ES, FR, US, etc.
	Country string `gorm:"size:2" json:"country"`

	// NumberType: geographic, mobile, toll_free, national, virtual.
	NumberType string `gorm:"size:30" json:"number_type"`

	// TelnyxNumberID is Telnyx's internal UUID for this number, useful for
	// API calls that target a specific number.
	TelnyxNumberID string `gorm:"size:100" json:"telnyx_number_id"`

	// IVRFlowID is the inbound IVR flow that handles incoming calls to this
	// number. Reuses the existing IVR engine — same flows that work for
	// WhatsApp calls also work for PSTN calls.
	IVRFlowID *uuid.UUID `gorm:"type:uuid" json:"ivr_flow_id,omitempty"`

	// IsActive: when false, calls to this number are rejected at the webhook
	// layer (useful for vacation, maintenance, etc.).
	IsActive bool `gorm:"default:true" json:"is_active"`

	// Recording: when true, every inbound call on this number is recorded
	// (Telnyx-side recording, audio file pulled to iReparo storage).
	RecordingEnabled bool `gorm:"default:false" json:"recording_enabled"`

	CreatedByID *uuid.UUID `gorm:"type:uuid" json:"created_by_id,omitempty"`
	UpdatedByID *uuid.UUID `gorm:"type:uuid" json:"updated_by_id,omitempty"`

	// Relations
	Organization *Organization     `gorm:"foreignKey:OrganizationID" json:"organization,omitempty"`
	Connection   *TelnyxConnection `gorm:"foreignKey:ConnectionID" json:"connection,omitempty"`
	IVRFlow      *IVRFlow          `gorm:"foreignKey:IVRFlowID" json:"ivr_flow,omitempty"`
	CreatedBy    *User             `gorm:"foreignKey:CreatedByID" json:"created_by,omitempty"`
	UpdatedBy    *User             `gorm:"foreignKey:UpdatedByID" json:"updated_by,omitempty"`
}

func (TelnyxNumber) TableName() string {
	return "telnyx_numbers"
}
