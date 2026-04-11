package telnyx

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Telnyx signs every webhook payload with Ed25519 using the public key shown
// in the Telnyx panel for each Call Control Application. We verify the
// signature on every incoming webhook to make sure it really comes from
// Telnyx and not a spoofed request to our public webhook URL.
//
// Spec: https://developers.telnyx.com/docs/api/v2/overview/webhooks#webhook-signing
//
// Headers Telnyx sends:
//
//   Telnyx-Signature-Ed25519: <base64-encoded signature>
//   Telnyx-Timestamp: <unix seconds when Telnyx generated the request>
//
// The signed message is exactly:  "<timestamp>|<raw_request_body>"
// (a literal pipe character between them, no JSON parsing).

const (
	// HeaderSignature is the HTTP header name carrying the Ed25519 signature.
	HeaderSignature = "Telnyx-Signature-Ed25519"
	// HeaderTimestamp is the HTTP header name carrying the Unix timestamp.
	HeaderTimestamp = "Telnyx-Timestamp"
	// MaxClockSkew is the maximum allowed difference between the timestamp
	// in the header and the receiver's clock. Older requests are rejected
	// to prevent replay attacks.
	MaxClockSkew = 5 * time.Minute
)

// VerifyWebhook checks the Ed25519 signature on a Telnyx webhook request.
//
//	publicKeyB64 — base64-encoded Ed25519 public key (32 bytes raw),
//	               from the Telnyx Call Control App page.
//	signatureB64 — value of the Telnyx-Signature-Ed25519 header.
//	timestampStr — value of the Telnyx-Timestamp header (Unix seconds).
//	body         — raw request body bytes (do NOT json.Unmarshal first).
//
// Returns nil on success or a non-nil error explaining what failed
// (clock skew, decode error, signature mismatch, etc.).
func VerifyWebhook(publicKeyB64, signatureB64, timestampStr string, body []byte) error {
	if publicKeyB64 == "" {
		return errors.New("telnyx: empty public key")
	}
	if signatureB64 == "" {
		return errors.New("telnyx: missing signature header")
	}
	if timestampStr == "" {
		return errors.New("telnyx: missing timestamp header")
	}

	// Decode the public key. Telnyx stores it as base64 of the raw 32-byte
	// Ed25519 public key.
	pubKey, err := base64.StdEncoding.DecodeString(publicKeyB64)
	if err != nil {
		return fmt.Errorf("telnyx: decode public key: %w", err)
	}
	if len(pubKey) != ed25519.PublicKeySize {
		return fmt.Errorf("telnyx: public key has wrong size: got %d, want %d", len(pubKey), ed25519.PublicKeySize)
	}

	// Decode the signature.
	sig, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("telnyx: decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("telnyx: signature has wrong size: got %d, want %d", len(sig), ed25519.SignatureSize)
	}

	// Reject stale timestamps to prevent replay attacks.
	tsUnix, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return fmt.Errorf("telnyx: invalid timestamp: %w", err)
	}
	tsTime := time.Unix(tsUnix, 0)
	skew := time.Since(tsTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		return fmt.Errorf("telnyx: timestamp out of allowed window (skew=%s, max=%s)", skew, MaxClockSkew)
	}

	// Build the signed message: "<timestamp>|<body>" as a single byte slice
	// without copying body more than necessary.
	msg := make([]byte, 0, len(timestampStr)+1+len(body))
	msg = append(msg, []byte(timestampStr)...)
	msg = append(msg, '|')
	msg = append(msg, body...)

	if !ed25519.Verify(ed25519.PublicKey(pubKey), msg, sig) {
		return errors.New("telnyx: signature does not match")
	}
	return nil
}
