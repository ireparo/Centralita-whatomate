package crm

import (
	"strconv"
	"testing"
	"time"
)

func TestSignAndVerify_RoundTrip(t *testing.T) {
	secret := "super-secret-shared-with-the-laravel-crm"
	body := []byte(`{"event":"call.ringing","data":{"call_id":"abc","caller_phone":"34637000111"}}`)
	ts := time.Now().Unix()

	sig := SignPayload(secret, ts, body)
	if sig == "" {
		t.Fatal("SignPayload returned empty signature")
	}

	if err := VerifySignature(secret, sig, strconv.FormatInt(ts, 10), body); err != nil {
		t.Fatalf("VerifySignature returned error on a fresh round-trip: %v", err)
	}
}

func TestVerifySignature_TamperedBody(t *testing.T) {
	secret := "shhh"
	body := []byte(`{"x":1}`)
	ts := time.Now().Unix()
	sig := SignPayload(secret, ts, body)

	// Mutate one byte: 1 -> 2.
	if err := VerifySignature(secret, sig, strconv.FormatInt(ts, 10), []byte(`{"x":2}`)); err == nil {
		t.Fatal("expected tampered body to be rejected")
	}
}

func TestVerifySignature_WrongSecret(t *testing.T) {
	body := []byte(`{"a":"b"}`)
	ts := time.Now().Unix()
	sig := SignPayload("alpha", ts, body)

	if err := VerifySignature("beta", sig, strconv.FormatInt(ts, 10), body); err == nil {
		t.Fatal("expected wrong secret to be rejected")
	}
}

func TestVerifySignature_StaleTimestamp(t *testing.T) {
	secret := "shhh"
	body := []byte(`{}`)
	staleTS := time.Now().Add(-30 * time.Minute).Unix()
	sig := SignPayload(secret, staleTS, body)

	if err := VerifySignature(secret, sig, strconv.FormatInt(staleTS, 10), body); err == nil {
		t.Fatal("expected stale timestamp to be rejected")
	}
}

func TestVerifySignature_MissingFields(t *testing.T) {
	body := []byte(`{}`)
	cases := []struct {
		name      string
		secret    string
		signature string
		ts        string
	}{
		{"empty secret", "", "sha256=abc", "1"},
		{"empty signature", "shhh", "", "1"},
		{"empty timestamp", "shhh", "sha256=abc", ""},
		{"non-numeric timestamp", "shhh", "sha256=abc", "not-a-number"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := VerifySignature(c.secret, c.signature, c.ts, body); err == nil {
				t.Errorf("expected error for case %q, got nil", c.name)
			}
		})
	}
}

func TestSignPayload_StableOutput(t *testing.T) {
	secret := "shhh"
	body := []byte(`hello`)
	ts := int64(1700000000)
	got1 := SignPayload(secret, ts, body)
	got2 := SignPayload(secret, ts, body)
	if got1 != got2 {
		t.Errorf("SignPayload not deterministic: %s vs %s", got1, got2)
	}
}
