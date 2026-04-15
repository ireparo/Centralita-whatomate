package handlers

import (
	"testing"

	"github.com/shridarpatil/whatomate/internal/integrations/crm"
)

func TestCRMEnrichBroadcast_KnownCaller(t *testing.T) {
	payload := map[string]any{
		"contact_name": "John",
	}
	lookup := &crm.LookupResponse{
		Found:           true,
		NormalizedPhone: "34637111222",
		Customer: &crm.Customer{
			ID:                 42,
			Name:               "Ana García",
			ProfileURL:         "https://sat.ireparo.es/admin/customers/42/edit",
			ActiveTicketsCount: 2,
			TotalSpentEUR:      385.50,
			VIP:                true,
			LastTicket: &crm.TicketRef{
				ID:            157,
				TrackingToken: "tok",
				Status:        "in_repair",
				Device:        "iPhone 15",
				URL:           "https://sat.ireparo.es/admin/repair-tickets/157/edit",
			},
		},
	}

	CRMEnrichBroadcast(payload, lookup)

	if payload["crm_lookup_attempted"] != true {
		t.Errorf("expected crm_lookup_attempted=true")
	}
	if payload["crm_customer_id"] != int64(42) {
		t.Errorf("expected crm_customer_id=42, got %v", payload["crm_customer_id"])
	}
	if payload["crm_customer_name"] != "Ana García" {
		t.Errorf("expected Ana García, got %v", payload["crm_customer_name"])
	}
	if payload["crm_vip"] != true {
		t.Errorf("expected VIP=true")
	}
	if payload["crm_active_tickets_count"] != 2 {
		t.Errorf("expected tickets=2")
	}
	if _, ok := payload["crm_last_ticket"]; !ok {
		t.Errorf("expected crm_last_ticket to be present")
	}
	// crm_create_url must NOT be present when caller is found
	if _, ok := payload["crm_create_url"]; ok {
		t.Errorf("crm_create_url should not be present when caller is found")
	}
}

func TestCRMEnrichBroadcast_UnknownCaller(t *testing.T) {
	payload := map[string]any{}
	lookup := &crm.LookupResponse{
		Found:           false,
		NormalizedPhone: "34637999888",
		CreateURL:       "https://sat.ireparo.es/admin/customers/create?phone=34637999888",
	}

	CRMEnrichBroadcast(payload, lookup)

	if payload["crm_lookup_attempted"] != true {
		t.Errorf("expected crm_lookup_attempted=true for unknown caller")
	}
	if payload["crm_create_url"] != "https://sat.ireparo.es/admin/customers/create?phone=34637999888" {
		t.Errorf("expected create URL, got %v", payload["crm_create_url"])
	}
	// No customer fields when unknown
	for _, k := range []string{"crm_customer_id", "crm_customer_name", "crm_profile_url"} {
		if _, ok := payload[k]; ok {
			t.Errorf("%s should not be present for unknown caller", k)
		}
	}
}

func TestCRMEnrichBroadcast_NilLookup(t *testing.T) {
	payload := map[string]any{"contact_name": "X"}

	CRMEnrichBroadcast(payload, nil)

	// Nothing added, including the lookup_attempted flag
	if _, ok := payload["crm_lookup_attempted"]; ok {
		t.Errorf("crm_lookup_attempted should be absent when lookup is nil")
	}
	if len(payload) != 1 {
		t.Errorf("payload should be untouched, got %v", payload)
	}
}

func TestCRMEnrichBroadcast_NilPayload(t *testing.T) {
	// Must not panic
	CRMEnrichBroadcast(nil, &crm.LookupResponse{Found: true})
}

func TestCRMEnrichBroadcast_UnknownCallerWithoutCreateURL(t *testing.T) {
	// If the CRM returned a lookup with Found=false but an empty CreateURL
	// for some reason, we should still set the lookup_attempted flag but
	// skip the create URL key.
	payload := map[string]any{}
	lookup := &crm.LookupResponse{Found: false, CreateURL: ""}

	CRMEnrichBroadcast(payload, lookup)

	if payload["crm_lookup_attempted"] != true {
		t.Errorf("expected crm_lookup_attempted=true")
	}
	if _, ok := payload["crm_create_url"]; ok {
		t.Errorf("crm_create_url should be absent when empty")
	}
}
