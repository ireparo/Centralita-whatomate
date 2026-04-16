// Package crm contains the iReparo PBX side of the integration with the
// external Laravel CRM (https://sat.ireparo.es).
//
// The CRM is the source of truth for customers; the PBX is the source of
// truth for calls and conversations. This package exposes:
//
//   - A typed HTTP client for the CRM's REST endpoints (lookup + events).
//   - HMAC-SHA256 signing of outbound event POSTs (replay-protected with
//     a Unix timestamp).
//   - An in-memory TTL cache so the PBX does not hit the CRM more than
//     once per call ringing.
//   - A persistent retry queue (table crm_event_queue) so events that
//     fail to deliver get retried with exponential backoff and never get
//     dropped silently.
package crm

import "strings"

// NormalizePhone converts an arbitrary phone string into the canonical
// E.164-without-plus format the CRM expects.
//
// Mirrors the Laravel side: preg_replace('/\D/', '', $phone), with the
// extra rule of stripping a leading "00" (international prefix used in
// some European number formats).
//
// Examples:
//
//	+34 873 94 07 02   →  34873940702
//	+34 (873) 940-702  →  34873940702
//	0034873940702      →  34873940702
//	34873940702        →  34873940702
//	  34   873 940 702 →  34873940702
//	"" / nil           →  ""
func NormalizePhone(phone string) string {
	var b strings.Builder
	b.Grow(len(phone))
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	out = strings.TrimPrefix(out, "00")
	return out
}
