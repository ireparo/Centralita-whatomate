package telnyx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

// TestVerifyWebhook_RoundTrip generates a fresh Ed25519 keypair, signs a
// payload exactly as Telnyx would, and verifies it round-trips.
func TestVerifyWebhook_RoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	body := []byte(`{"data":{"event_type":"call.initiated","id":"x","occurred_at":"2026-04-10T00:00:00Z","payload":{}}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	msg := append([]byte(ts+"|"), body...)
	sig := ed25519.Sign(priv, msg)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	if err := VerifyWebhook(pubB64, sigB64, ts, body); err != nil {
		t.Fatalf("expected valid signature, got error: %v", err)
	}
}

// TestVerifyWebhook_StaleTimestamp ensures replay protection rejects
// timestamps outside the allowed window.
func TestVerifyWebhook_StaleTimestamp(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	body := []byte(`{"data":{}}`)
	staleTS := strconv.FormatInt(time.Now().Add(-30*time.Minute).Unix(), 10)
	msg := append([]byte(staleTS+"|"), body...)
	sigB64 := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))

	if err := VerifyWebhook(pubB64, sigB64, staleTS, body); err == nil {
		t.Fatal("expected stale timestamp to be rejected, but it was accepted")
	}
}

// TestVerifyWebhook_TamperedBody ensures even a 1-byte mutation invalidates
// the signature.
func TestVerifyWebhook_TamperedBody(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	body := []byte(`{"data":{"x":1}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sigB64 := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(ts+"|"), body...)))

	tampered := []byte(`{"data":{"x":2}}`) // single digit changed
	if err := VerifyWebhook(pubB64, sigB64, ts, tampered); err == nil {
		t.Fatal("expected tampered body to fail signature, but verification passed")
	}
}

// TestVerifyWebhook_WrongKey ensures verification fails with a different
// public key (a request signed by attacker's keypair).
func TestVerifyWebhook_WrongKey(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	otherPubB64 := base64.StdEncoding.EncodeToString(otherPub)

	body := []byte(`{"data":{}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sigB64 := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, append([]byte(ts+"|"), body...)))

	if err := VerifyWebhook(otherPubB64, sigB64, ts, body); err == nil {
		t.Fatal("expected wrong-key verification to fail")
	}
}

// TestVerifyWebhook_MissingHeaders covers degenerate inputs.
func TestVerifyWebhook_MissingHeaders(t *testing.T) {
	cases := []struct {
		name           string
		pub, sig, ts   string
	}{
		{"empty pub", "", "x", "1"},
		{"empty sig", "AAAA", "", "1"},
		{"empty ts", "AAAA", "x", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := VerifyWebhook(c.pub, c.sig, c.ts, []byte("{}")); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}
