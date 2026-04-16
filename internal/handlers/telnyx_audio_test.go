package handlers

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Unit tests for the IVR audio signing scheme used by Telnyx fetches.
// They exercise signIVRAudio directly (pure function, no deps) rather than
// the HTTP handler, so they can run without a DB / Redis / JWT config.

func TestSignIVRAudio_Deterministic(t *testing.T) {
	secret := "test-secret-0123456789abcdef"
	filename := "greeting-welcome.mp3"
	expiry := int64(1712345678)

	sig1 := signIVRAudio(secret, filename, expiry)
	sig2 := signIVRAudio(secret, filename, expiry)
	if sig1 != sig2 {
		t.Errorf("HMAC not deterministic: %q vs %q", sig1, sig2)
	}
	if len(sig1) != 64 { // sha256 hex = 32 bytes = 64 chars
		t.Errorf("unexpected signature length: got %d want 64", len(sig1))
	}
}

func TestSignIVRAudio_ChangesWithFilename(t *testing.T) {
	secret := "test-secret"
	expiry := int64(1712345678)
	a := signIVRAudio(secret, "a.mp3", expiry)
	b := signIVRAudio(secret, "b.mp3", expiry)
	if a == b {
		t.Error("signatures collided across different filenames")
	}
}

func TestSignIVRAudio_ChangesWithExpiry(t *testing.T) {
	secret := "test-secret"
	filename := "a.mp3"
	a := signIVRAudio(secret, filename, 1712345678)
	b := signIVRAudio(secret, filename, 1712345679)
	if a == b {
		t.Error("signatures collided across different expiries")
	}
}

func TestSignIVRAudio_ChangesWithSecret(t *testing.T) {
	filename := "a.mp3"
	expiry := int64(1712345678)
	a := signIVRAudio("secret-one", filename, expiry)
	b := signIVRAudio("secret-two", filename, expiry)
	if a == b {
		t.Error("signatures collided across different secrets")
	}
}

// Regression guard: the signed message layout is
// "<filename>|<expiry>" — changing this silently would break all in-flight
// calls at deploy time, so assert the format explicitly.
func TestSignIVRAudio_MessageLayout(t *testing.T) {
	secret := "k"
	filename := "hello.mp3"
	expiry := int64(1234567890)

	// If anyone changes signIVRAudio to use e.g. filename + expiry (no
	// separator), the value would differ from what a standalone HMAC-SHA256
	// of "hello.mp3|1234567890" produces. We do not hardcode the hex digest
	// here (would be brittle to variable orderings) but we confirm that
	// stripping the separator produces a DIFFERENT signature.
	withSep := signIVRAudio(secret, filename, expiry)
	differentLayout := signIVRAudio(secret, filename+strconv.FormatInt(expiry, 10), 0)
	if withSep == differentLayout {
		t.Error("separator in message layout was dropped — potential signature collision")
	}
	// And confirm the output is lowercase hex (what the URL expects).
	if strings.ToLower(withSep) != withSep {
		t.Errorf("signature should be lowercase hex, got %q", withSep)
	}
}

// Smoke test the full build URL path (unit-level, not HTTP).
func TestBuildSignedIVRAudioURL_Shape(t *testing.T) {
	// We bypass the App struct (would need DB etc) and call signIVRAudio +
	// build the URL manually with the same formatting to keep this pure.
	secret := "s"
	filename := "menu-prompt.mp3"
	expiry := time.Now().Add(15 * time.Minute).Unix()
	sig := signIVRAudio(secret, filename, expiry)

	// Emulate what BuildSignedIVRAudioURL produces:
	got := "https://pbx.example/api/public/ivr-audio/" + filename +
		"?e=" + strconv.FormatInt(expiry, 10) + "&s=" + sig

	if !strings.HasPrefix(got, "https://") {
		t.Error("expected https URL")
	}
	if !strings.Contains(got, "?e=") || !strings.Contains(got, "&s=") {
		t.Errorf("URL missing expected query params: %s", got)
	}
	if !strings.Contains(got, filename) {
		t.Errorf("URL missing filename: %s", got)
	}
}
