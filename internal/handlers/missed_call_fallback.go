package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shridarpatil/whatomate/internal/models"
	"github.com/shridarpatil/whatomate/pkg/whatsapp"
)

// MissedCallFallbackReason describes why the fallback was skipped (for logs).
type MissedCallFallbackReason string

const (
	missedCallFallbackDisabled       MissedCallFallbackReason = "feature_disabled"
	missedCallFallbackNoTemplate     MissedCallFallbackReason = "no_template_configured"
	missedCallFallbackTemplateNotOk  MissedCallFallbackReason = "template_not_approved"
	missedCallFallbackNoContact      MissedCallFallbackReason = "contact_missing"
	missedCallFallbackNoPhone        MissedCallFallbackReason = "contact_no_phone"
	missedCallFallbackMarketingOpted MissedCallFallbackReason = "marketing_opt_out"
	missedCallFallbackNoAccount      MissedCallFallbackReason = "whatsapp_account_missing"
)

// TriggerMissedCallWhatsApp launches the missed-call WhatsApp fallback in the
// background so it never blocks webhook processing. If the feature is disabled,
// or any precondition fails, the call is a silent no-op (reason is logged at
// debug level).
//
// The fallback fires whenever a call (incoming or outgoing, any channel) ends
// with status "missed" and the contact is known. Behaviour is controlled by two
// org-level settings stored in Organization.Settings (JSONB):
//
//   - missed_call_whatsapp_enabled      (bool)
//   - missed_call_whatsapp_template_id  (string UUID of an approved Template)
//
// The sent message is persisted as a regular outgoing Message row with
// metadata.missed_call_fallback = true, so it shows up in the chat history and
// the agent can see what the contact was told automatically.
func (a *App) TriggerMissedCallWhatsApp(callLog *models.CallLog) {
	if callLog == nil || a == nil || a.WhatsApp == nil {
		return
	}
	// Snapshot the fields we need so the goroutine does not rely on the
	// caller keeping the pointer alive.
	snap := *callLog

	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := a.sendMissedCallWhatsApp(ctx, &snap); err != nil {
			a.Log.Debug("Missed-call WhatsApp fallback skipped",
				"call_log_id", snap.ID,
				"direction", snap.Direction,
				"reason", err.Error())
		}
	}()
}

// sendMissedCallWhatsApp performs the synchronous part of the fallback. It is
// called from a goroutine launched by TriggerMissedCallWhatsApp.
func (a *App) sendMissedCallWhatsApp(ctx context.Context, callLog *models.CallLog) error {
	// 1. Load organization and check the feature toggle.
	var org models.Organization
	if err := a.DB.Where("id = ?", callLog.OrganizationID).First(&org).Error; err != nil {
		return fmt.Errorf("load organization: %w", err)
	}
	if org.Settings == nil {
		return errors.New(string(missedCallFallbackDisabled))
	}
	enabled, _ := org.Settings["missed_call_whatsapp_enabled"].(bool)
	if !enabled {
		return errors.New(string(missedCallFallbackDisabled))
	}

	templateIDRaw, _ := org.Settings["missed_call_whatsapp_template_id"].(string)
	if templateIDRaw == "" {
		return errors.New(string(missedCallFallbackNoTemplate))
	}
	templateID, err := uuid.Parse(templateIDRaw)
	if err != nil {
		return fmt.Errorf("%s: %w", missedCallFallbackNoTemplate, err)
	}

	// 2. Load the configured template and make sure it belongs to the org
	// and has been approved by Meta.
	var template models.Template
	if err := a.DB.Where("id = ? AND organization_id = ?", templateID, callLog.OrganizationID).
		First(&template).Error; err != nil {
		return fmt.Errorf("load template: %w", err)
	}
	if !strings.EqualFold(template.Status, "APPROVED") {
		return fmt.Errorf("%s: status=%s", missedCallFallbackTemplateNotOk, template.Status)
	}

	// 3. Resolve the contact. We refuse to send a WhatsApp message to an
	// unknown caller because we need a phone number and opt-out status.
	if callLog.ContactID == uuid.Nil {
		return errors.New(string(missedCallFallbackNoContact))
	}
	var contact models.Contact
	if err := a.DB.Where("id = ?", callLog.ContactID).First(&contact).Error; err != nil {
		return fmt.Errorf("load contact: %w", err)
	}
	if contact.PhoneNumber == "" {
		return errors.New(string(missedCallFallbackNoPhone))
	}
	if contact.MarketingOptOut && strings.EqualFold(template.Category, "MARKETING") {
		return errors.New(string(missedCallFallbackMarketingOpted))
	}

	// 4. Pick the WhatsApp account to send from. Prefer the account the
	// template was built for; fall back to the account the call arrived on.
	accountName := template.WhatsAppAccount
	if accountName == "" {
		accountName = callLog.WhatsAppAccount
	}
	if accountName == "" {
		return errors.New(string(missedCallFallbackNoAccount))
	}
	var account models.WhatsAppAccount
	if err := a.DB.Where("name = ? AND organization_id = ?", accountName, callLog.OrganizationID).
		First(&account).Error; err != nil {
		return fmt.Errorf("%s: %w", missedCallFallbackNoAccount, err)
	}
	a.decryptAccountSecrets(&account)

	// 5. Build components. We do not try to fill in body params here: the
	// admin is expected to pick a template whose body either has no
	// placeholders or uses {{1}}=contact name (auto-filled below). A future
	// iteration can support a richer variable mapping.
	bodyParams := map[string]string{}
	if contact.ProfileName != "" {
		bodyParams["1"] = contact.ProfileName
	}
	components := whatsapp.BuildTemplateComponents(bodyParams, template.HeaderType, "")
	components = append(components, whatsapp.AutoButtonComponents(template.Buttons)...)

	rcpt := whatsapp.Recipient{Phone: contact.PhoneNumber}
	waAccount := account.ToWAAccount()
	waMessageID, sendErr := a.WhatsApp.SendTemplateMessage(
		ctx, waAccount, rcpt, template.Name, template.Language, components,
	)

	// 6. Persist the message so it appears in the chat thread. We create
	// the row even on failure so the agent can see the attempted delivery.
	message := models.Message{
		OrganizationID:    callLog.OrganizationID,
		WhatsAppAccount:   accountName,
		ContactID:         contact.ID,
		WhatsAppMessageID: waMessageID,
		Direction:         models.DirectionOutgoing,
		MessageType:       models.MessageTypeTemplate,
		TemplateName:      template.Name,
		Content:           template.BodyContent,
		Metadata: models.JSONB{
			"missed_call_fallback": true,
			"call_log_id":          callLog.ID.String(),
			"call_direction":       string(callLog.Direction),
		},
	}
	if sendErr != nil {
		message.Status = models.MessageStatusFailed
		message.ErrorMessage = sendErr.Error()
	} else {
		message.Status = models.MessageStatusSent
	}
	if dbErr := a.DB.Create(&message).Error; dbErr != nil {
		a.Log.Error("Failed to persist missed-call fallback message",
			"error", dbErr, "call_log_id", callLog.ID)
	}

	if sendErr != nil {
		return fmt.Errorf("send template: %w", sendErr)
	}

	a.Log.Info("Missed-call WhatsApp fallback sent",
		"call_log_id", callLog.ID,
		"contact_id", contact.ID,
		"template", template.Name,
		"whatsapp_message_id", waMessageID)
	return nil
}
