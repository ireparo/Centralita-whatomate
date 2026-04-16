package handlers

import (
	"testing"
)

// Tests for the click-to-call state marker. These are pure functions (no
// DB, no HTTP) so they run without the full test harness.

func TestEncodeDecodeClickToCallState_RoundTrip(t *testing.T) {
	original := &ClickToCallState{
		CallLogID:   "8e0a7b02-3d4c-4e5f-6789-abcdef012345",
		TargetPhone: "34666112233",
		FromPhone:   "34873940702",
		OrgID:       "11111111-2222-3333-4444-555555555555",
		InitiatedBy: "22222222-3333-4444-5555-666666666666",
	}

	encoded, err := EncodeClickToCallState(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if encoded == "" {
		t.Fatal("encoded state is empty")
	}
	if original.Kind != "c2c_agent" {
		t.Error("Kind should have been set to c2c_agent on encode")
	}

	decoded, err := DecodeClickToCallState(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded == nil {
		t.Fatal("decoded state is nil")
	}
	if decoded.CallLogID != original.CallLogID {
		t.Errorf("CallLogID mismatch: got %q want %q", decoded.CallLogID, original.CallLogID)
	}
	if decoded.TargetPhone != original.TargetPhone {
		t.Errorf("TargetPhone mismatch: got %q want %q", decoded.TargetPhone, original.TargetPhone)
	}
	if decoded.FromPhone != original.FromPhone {
		t.Errorf("FromPhone mismatch: got %q want %q", decoded.FromPhone, original.FromPhone)
	}
	if decoded.InitiatedBy != original.InitiatedBy {
		t.Errorf("InitiatedBy mismatch: got %q want %q", decoded.InitiatedBy, original.InitiatedBy)
	}
}

func TestDecodeClickToCallState_NotOurMarker(t *testing.T) {
	// Simulate an IVR state (has a different "k" / no "k" at all) being
	// passed to the click-to-call decoder. It must return (nil, nil) so
	// the webhook handler falls through to the IVR dispatcher.
	ivrLikeEncoded := "eyJrIjoic29tZXRoaW5nX2Vsc2UiLCJjbG9nIjoiYWJjIn0=" // {"k":"something_else","clog":"abc"}
	decoded, err := DecodeClickToCallState(ivrLikeEncoded)
	if err != nil {
		t.Fatalf("should not error on unknown kind, got: %v", err)
	}
	if decoded != nil {
		t.Errorf("expected nil for unknown kind, got %+v", decoded)
	}
}

func TestDecodeClickToCallState_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"invalid base64", "not-base64-!!!"},
		{"valid base64 but not json", "aGVsbG8="}, // "hello"
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeClickToCallState(c.in); err == nil {
				t.Errorf("expected error for %q, got nil", c.in)
			}
		})
	}
}

func TestClassifyHangupSource(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"caller", "agent"},    // the leg we initiated is "our" caller
		{"callee", "client"},   // the customer
		{"CALLER", "agent"},    // case-insensitive
		{"call_control_app", "system"},
		{"client", "system"},
		{"", "system"}, // unknown
		{"weird", "system"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := classifyHangupSource(c.in)
			if string(got) != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}
