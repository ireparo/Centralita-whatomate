package calling

import (
	"testing"

	"github.com/google/uuid"
)

func TestEncodeDecodeTelnyxIVRState_RoundTrip(t *testing.T) {
	original := &TelnyxIVRState{
		CallLogID:   uuid.MustParse("8e0a7b02-3d4c-4e5f-6789-abcdef012345"),
		IVRFlowID:   uuid.MustParse("11111111-2222-3333-4444-555555555555"),
		CurrentNode: "node_greeting_1",
		Path:        []string{"node_greeting_1", "node_menu_2"},
	}

	encoded, err := EncodeTelnyxIVRState(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if encoded == "" {
		t.Fatal("encoded state is empty")
	}

	decoded, err := DecodeTelnyxIVRState(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.CallLogID != original.CallLogID {
		t.Errorf("CallLogID mismatch: got %v want %v", decoded.CallLogID, original.CallLogID)
	}
	if decoded.IVRFlowID != original.IVRFlowID {
		t.Errorf("IVRFlowID mismatch: got %v want %v", decoded.IVRFlowID, original.IVRFlowID)
	}
	if decoded.CurrentNode != original.CurrentNode {
		t.Errorf("CurrentNode mismatch: got %q want %q", decoded.CurrentNode, original.CurrentNode)
	}
	if len(decoded.Path) != len(original.Path) {
		t.Errorf("Path length mismatch: got %d want %d", len(decoded.Path), len(original.Path))
	}
}

func TestDecodeTelnyxIVRState_Errors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"invalid base64", "not-base64-!!!"},
		{"valid base64 but not json", "aGVsbG8gd29ybGQ="}, // "hello world"
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := DecodeTelnyxIVRState(c.in); err == nil {
				t.Errorf("expected error for input %q, got nil", c.in)
			}
		})
	}
}

func TestPhoneToE164(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"+34 873 94 07 02", "34873940702"},
		{"+34 (873) 940-702", "34873940702"},
		{"34873940702", "34873940702"},
		{"0034873940702", "34873940702"},
		{"+1-555-123-4567", "15551234567"},
		{"   34   873  940 702  ", "34873940702"},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got := PhoneToE164(c.in)
			if got != c.want {
				t.Errorf("PhoneToE164(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestMapTelnyxOutcome(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   map[string]any
		want      string
	}{
		{
			name:      "playback ended → default",
			eventType: "call.playback.ended",
			payload:   map[string]any{},
			want:      "default",
		},
		{
			name:      "gather ended with digits",
			eventType: "call.gather.ended",
			payload:   map[string]any{"digits": "3"},
			want:      "digit:3",
		},
		{
			name:      "gather ended timeout",
			eventType: "call.gather.ended",
			payload:   map[string]any{"digits": ""},
			want:      "timeout",
		},
		{
			name:      "dtmf received",
			eventType: "call.dtmf.received",
			payload:   map[string]any{"digit": "5"},
			want:      "digit:5",
		},
		{
			name:      "bridged",
			eventType: "call.bridged",
			payload:   map[string]any{},
			want:      "default",
		},
		{
			name:      "unknown",
			eventType: "call.something.weird",
			payload:   map[string]any{},
			want:      "default",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MapTelnyxOutcome(c.eventType, c.payload)
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestLoadIVRFlowGraph_NilInputs(t *testing.T) {
	if _, err := LoadIVRFlowGraph(nil); err == nil {
		t.Error("expected error for nil flow")
	}
}
