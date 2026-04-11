package crm

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"
)

// HMAC-SHA256 signing scheme used for outbound POSTs to the CRM.
//
// The signature header sent with every POST is:
//
//	X-iReparo-Signature: sha256=<hex>
//	X-iReparo-Timestamp: <unix-epoch-seconds>
//
// The signed message is exactly:
//
//	"<timestamp>.<raw_request_body>"
//
// (a literal '.' separator, no JSON parsing).
//
// On the CRM side (Laravel), verification looks like:
//
//	$expected = hash_hmac('sha256', $timestamp.'.'.$rawBody, config('ireparo.pbx_webhook_secret'));
//	if (! hash_equals($expected, str_replace('sha256=', '', $signatureHeader))) {
//	    abort(403);
//	}
//
// We use a 5-minute clock skew window for replay protection (matches
// Telnyx convention; see internal/integrations/telnyx/signing.go).

// HeaderSignature is the HTTP header carrying the HMAC signature.
const HeaderSignature = "X-iReparo-Signature"

// HeaderTimestamp is the HTTP header carrying the Unix timestamp.
const HeaderTimestamp = "X-iReparo-Timestamp"

// HeaderEventType is the HTTP header carrying the event type, redundant
// with the JSON payload but useful for the CRM to route quickly.
const HeaderEventType = "X-iReparo-Event"

// HeaderAPIKey is the HTTP header carrying the per-deployment shared API
// key. Used for read endpoints (lookup) AND for write endpoints alongside
// the HMAC signature.
const HeaderAPIKey = "X-iReparo-Api-Key"

// MaxClockSkew is the maximum delta tolerated between the timestamp in the
// header and the receiver's clock when verifying. The CRM enforces this on
// its end; we expose the constant so it stays in sync via shared spec.
const MaxClockSkew = 5 * time.Minute

// SignPayload computes the HMAC-SHA256 of "<timestamp>.<body>" using the
// supplied secret. Returns the hex digest prefixed with "sha256=" so it
// can be assigned directly to the X-iReparo-Signature header.
func SignPayload(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks that the supplied signature header matches the
// HMAC of timestamp+body computed with secret, AND that the timestamp is
// within the allowed clock skew window.
//
// This function is exported so the CRM-Spec test suite (or any internal
// tooling that wants to replay events) can verify locally.
func VerifySignature(secret string, signatureHeader string, timestampStr string, body []byte) error {
	if secret == "" {
		return errors.New("crm: empty signing secret")
	}
	if signatureHeader == "" {
		return errors.New("crm: missing signature header")
	}
	if timestampStr == "" {
		return errors.New("crm: missing timestamp header")
	}

	tsUnix, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return errors.New("crm: invalid timestamp")
	}
	tsTime := time.Unix(tsUnix, 0)
	skew := time.Since(tsTime)
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxClockSkew {
		return errors.New("crm: timestamp out of allowed window")
	}

	expected := SignPayload(secret, tsUnix, body)
	if !hmac.Equal([]byte(expected), []byte(signatureHeader)) {
		return errors.New("crm: signature does not match")
	}
	return nil
}
