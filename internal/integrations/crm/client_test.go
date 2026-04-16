package crm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient_Lookup_Found(t *testing.T) {
	// Mock CRM that always returns the same customer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pbx/lookup" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get(HeaderAPIKey) != "test-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(LookupResponse{
			Found:           true,
			NormalizedPhone: r.URL.Query().Get("phone"),
			Customer:        &Customer{ID: 1234, Name: "Juan Pérez", Phone: "34637000111"},
		})
	}))
	defer srv.Close()

	client := NewClient(Config{
		Enabled: true,
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, nil)

	resp, err := client.Lookup(context.Background(), "+34 637 000 111")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if !resp.Found || resp.Customer == nil || resp.Customer.ID != 1234 {
		t.Errorf("unexpected response: %+v", resp)
	}
	if resp.NormalizedPhone != "34637000111" {
		t.Errorf("phone normalization failed: got %q", resp.NormalizedPhone)
	}
}

func TestClient_Lookup_CachedOnSecondCall(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(LookupResponse{
			Found:           true,
			NormalizedPhone: "34637000111",
			Customer:        &Customer{ID: 1, Name: "X"},
		})
	}))
	defer srv.Close()

	client := NewClient(Config{Enabled: true, BaseURL: srv.URL, APIKey: "k"}, nil)
	for i := 0; i < 5; i++ {
		_, err := client.Lookup(context.Background(), "34637000111")
		if err != nil {
			t.Fatalf("Lookup error: %v", err)
		}
	}
	if calls != 1 {
		t.Errorf("expected 1 network call (4 cache hits), got %d", calls)
	}
}

func TestClient_Lookup_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(LookupResponse{
			Found:           false,
			NormalizedPhone: r.URL.Query().Get("phone"),
		})
	}))
	defer srv.Close()

	client := NewClient(Config{Enabled: true, BaseURL: srv.URL, APIKey: "k"}, nil)
	resp, err := client.Lookup(context.Background(), "34000000000")
	if err != nil {
		t.Fatalf("Lookup error: %v", err)
	}
	if resp.Found {
		t.Error("expected found=false")
	}
}

func TestClient_Send_Success(t *testing.T) {
	var captured struct {
		body      []byte
		signature string
		timestamp string
		eventType string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pbx/call-event" {
			http.NotFound(w, r)
			return
		}
		captured.body, _ = io.ReadAll(r.Body)
		captured.signature = r.Header.Get(HeaderSignature)
		captured.timestamp = r.Header.Get(HeaderTimestamp)
		captured.eventType = r.Header.Get(HeaderEventType)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := NewClient(Config{
		Enabled:       true,
		BaseURL:       srv.URL,
		APIKey:        "k",
		WebhookSecret: "shh",
	}, nil)

	env, err := client.BuildEvent(EventCallRinging, &CallRingingData{
		CallID:      "abc",
		Direction:   "incoming",
		CallerPhone: "34637000111",
		Channel:     "whatsapp",
	})
	if err != nil {
		t.Fatalf("BuildEvent: %v", err)
	}
	if err := client.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if !strings.HasPrefix(captured.signature, "sha256=") {
		t.Errorf("signature header missing or wrong: %q", captured.signature)
	}
	if captured.eventType != EventCallRinging {
		t.Errorf("event type header wrong: %q", captured.eventType)
	}
	// Verify the signature on the receiving side, like the CRM does.
	if err := VerifySignature("shh", captured.signature, captured.timestamp, captured.body); err != nil {
		t.Errorf("server-side signature verification failed: %v", err)
	}
}

func TestClient_Send_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := NewClient(Config{Enabled: true, BaseURL: srv.URL, APIKey: "k", WebhookSecret: "shh"}, nil)
	env, _ := client.BuildEvent(EventCallEnded, &CallEndedData{CallID: "x"})
	if err := client.Send(context.Background(), env); err == nil {
		t.Error("expected error on 500 response")
	}
}

func TestClient_Disabled(t *testing.T) {
	client := NewClient(Config{Enabled: false}, nil)
	if client.Enabled() {
		t.Error("expected client to report disabled")
	}
	if _, err := client.Lookup(context.Background(), "34637000111"); err == nil {
		t.Error("expected error when calling Lookup on disabled client")
	}
}

// TestClient_RespectsTimeout makes sure the client honors the per-request
// context timeout (so a slow CRM does not freeze the call pipeline).
func TestClient_RespectsTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(LookupResponse{Found: false, NormalizedPhone: "x"})
	}))
	defer srv.Close()

	client := NewClient(Config{Enabled: true, BaseURL: srv.URL, APIKey: "k"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.Lookup(ctx, "34637000111"); err == nil {
		t.Error("expected timeout error")
	}
}
