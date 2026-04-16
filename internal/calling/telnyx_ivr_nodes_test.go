package calling

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Tests for the pure (no-Telnyx) parts of the Phase 2.3 IVR node
// executors. The Telnyx-interacting parts (menu, gather, goto_flow) are
// covered by integration tests that run against a mocked Telnyx client.

func TestMenuValidDigits(t *testing.T) {
	cases := []struct {
		name    string
		options map[string]any
		wantSet map[byte]bool
	}{
		{
			name: "typical 1-2-3 menu",
			options: map[string]any{
				"1": map[string]any{"label": "Ventas"},
				"2": map[string]any{"label": "Soporte"},
				"3": map[string]any{"label": "Baja"},
			},
			wantSet: map[byte]bool{'1': true, '2': true, '3': true},
		},
		{
			name: "skips non-digit keys",
			options: map[string]any{
				"1":     map[string]any{},
				"label": map[string]any{},
				"2":     map[string]any{},
			},
			wantSet: map[byte]bool{'1': true, '2': true},
		},
		{
			name: "accepts * and #",
			options: map[string]any{
				"1": map[string]any{},
				"*": map[string]any{},
				"#": map[string]any{},
			},
			wantSet: map[byte]bool{'1': true, '*': true, '#': true},
		},
		{
			name:    "no options",
			options: nil,
			wantSet: map[byte]bool{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			node := &IVRNode{Type: IVRNodeMenu, Config: map[string]any{"options": c.options}}
			got := menuValidDigits(node)
			// Compare as sets since map iteration order is not guaranteed.
			gotSet := map[byte]bool{}
			for i := 0; i < len(got); i++ {
				gotSet[got[i]] = true
			}
			if len(gotSet) != len(c.wantSet) {
				t.Errorf("len mismatch: got %q want set %v", got, c.wantSet)
				return
			}
			for d := range c.wantSet {
				if !gotSet[d] {
					t.Errorf("missing digit %q in %q", d, got)
				}
			}
		})
	}
}

func TestPublicVariables_StripsInternalKeys(t *testing.T) {
	in := map[string]string{
		"pin":                "1234",
		"__gather_store_as":  "pin",
		"customer_name":      "Alice",
		"__internal_counter": "3",
	}
	out := publicVariables(in)
	if _, ok := out["__gather_store_as"]; ok {
		t.Error("internal key leaked")
	}
	if _, ok := out["__internal_counter"]; ok {
		t.Error("internal key leaked")
	}
	if out["pin"] != "1234" {
		t.Errorf("pin: got %q want %q", out["pin"], "1234")
	}
	if out["customer_name"] != "Alice" {
		t.Errorf("customer_name: got %q want %q", out["customer_name"], "Alice")
	}
}

func TestPublicVariables_NilInput(t *testing.T) {
	out := publicVariables(nil)
	if len(out) != 0 {
		t.Errorf("expected empty map, got %v", out)
	}
}

func TestGetConfigIntT(t *testing.T) {
	cfg := map[string]any{
		"int_value":     10,
		"int64_value":   int64(20),
		"float64_value": float64(30),
		"float32_value": float32(40),
		"string_value":  "not-a-number",
	}
	cases := []struct {
		key  string
		def  int
		want int
	}{
		{"int_value", 0, 10},
		{"int64_value", 0, 20},
		{"float64_value", 0, 30},
		{"float32_value", 0, 40},
		{"string_value", 99, 99}, // wrong type → default
		{"missing_key", 7, 7},    // missing → default
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			got := getConfigIntT(cfg, c.key, c.def)
			if got != c.want {
				t.Errorf("got %d want %d", got, c.want)
			}
		})
	}
}

func TestExecuteTelnyxTiming(t *testing.T) {
	// Wednesday 2026-04-15 at 14:00 local time
	wed14 := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC)
	// Wednesday 2026-04-15 at 21:00 (after hours)
	wed21 := time.Date(2026, 4, 15, 21, 0, 0, 0, time.UTC)
	// Sunday 2026-04-19 at 14:00 (closed day)
	sun14 := time.Date(2026, 4, 19, 14, 0, 0, 0, time.UTC)

	node := &IVRNode{Type: IVRNodeTiming, Config: map[string]any{
		"schedule": []any{
			map[string]any{"day": "wednesday", "enabled": true, "start_time": "09:00", "end_time": "18:00"},
			map[string]any{"day": "sunday", "enabled": false, "start_time": "00:00", "end_time": "00:00"},
		},
	}}

	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"weekday working hours", wed14, "in_hours"},
		{"weekday after hours", wed21, "out_of_hours"},
		{"closed day", sun14, "out_of_hours"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := executeTelnyxTiming(node, c.now)
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestExecuteTelnyxTiming_DayNotInSchedule(t *testing.T) {
	// Friday but only monday is configured → defaults to out_of_hours.
	fri := time.Date(2026, 4, 17, 14, 0, 0, 0, time.UTC)
	node := &IVRNode{Type: IVRNodeTiming, Config: map[string]any{
		"schedule": []any{
			map[string]any{"day": "monday", "enabled": true, "start_time": "09:00", "end_time": "18:00"},
		},
	}}
	if got := executeTelnyxTiming(node, fri); got != "out_of_hours" {
		t.Errorf("got %q want out_of_hours", got)
	}
}

func TestExecuteTelnyxTiming_MalformedSchedule(t *testing.T) {
	now := time.Date(2026, 4, 15, 14, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"no schedule", map[string]any{}, "out_of_hours"},
		{"empty schedule", map[string]any{"schedule": []any{}}, "out_of_hours"},
		{
			name: "garbled start_time",
			config: map[string]any{"schedule": []any{
				map[string]any{"day": "wednesday", "enabled": true, "start_time": "not-a-time", "end_time": "18:00"},
			}},
			want: "out_of_hours",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			node := &IVRNode{Type: IVRNodeTiming, Config: c.config}
			if got := executeTelnyxTiming(node, now); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestExecuteTelnyxHTTPCallback_2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert interpolation happened on the URL.
		if r.URL.Query().Get("customer_id") != "12345" {
			t.Errorf("query param not interpolated: got %q", r.URL.Query().Get("customer_id"))
		}
		// Assert interpolation happened on the header.
		if r.Header.Get("X-Customer") != "Alice" {
			t.Errorf("header not interpolated: got %q", r.Header.Get("X-Customer"))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	node := &IVRNode{Type: IVRNodeHTTPCallback, Config: map[string]any{
		"url":    server.URL + "/lookup?customer_id={{customer_id}}",
		"method": "GET",
		"headers": map[string]any{
			"X-Customer": "{{customer_name}}",
		},
		"timeout_seconds":   5,
		"response_store_as": "lookup_response",
	}}
	state := &TelnyxIVRState{
		Variables: map[string]string{
			"customer_id":   "12345",
			"customer_name": "Alice",
			// Internal key — MUST be stripped before interpolation.
			"__gather_store_as": "should_not_leak",
		},
	}

	got := executeTelnyxHTTPCallback(node, state)
	if got != "http:2xx" {
		t.Errorf("outcome: got %q want http:2xx", got)
	}
	if state.Variables["lookup_response"] != `{"ok":true}` {
		t.Errorf("response not stored: got %q", state.Variables["lookup_response"])
	}
}

func TestExecuteTelnyxHTTPCallback_Non2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	node := &IVRNode{Type: IVRNodeHTTPCallback, Config: map[string]any{
		"url":             server.URL,
		"method":          "GET",
		"timeout_seconds": 5,
	}}
	state := &TelnyxIVRState{}

	if got := executeTelnyxHTTPCallback(node, state); got != "http:non2xx" {
		t.Errorf("got %q want http:non2xx", got)
	}
}

func TestExecuteTelnyxHTTPCallback_MissingURL(t *testing.T) {
	node := &IVRNode{Type: IVRNodeHTTPCallback, Config: map[string]any{}}
	state := &TelnyxIVRState{}
	if got := executeTelnyxHTTPCallback(node, state); got != "http:non2xx" {
		t.Errorf("got %q want http:non2xx (missing URL)", got)
	}
}

func TestExecuteTelnyxHTTPCallback_NetworkError(t *testing.T) {
	node := &IVRNode{Type: IVRNodeHTTPCallback, Config: map[string]any{
		"url":             "http://127.0.0.1:1", // unreachable
		"timeout_seconds": 1,
	}}
	state := &TelnyxIVRState{}
	if got := executeTelnyxHTTPCallback(node, state); got != "http:non2xx" {
		t.Errorf("got %q want http:non2xx", got)
	}
}

func TestExecuteTelnyxHTTPCallback_LargeResponseCapped(t *testing.T) {
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(big)
	}))
	defer server.Close()

	node := &IVRNode{Type: IVRNodeHTTPCallback, Config: map[string]any{
		"url":               server.URL,
		"timeout_seconds":   5,
		"response_store_as": "big",
	}}
	state := &TelnyxIVRState{}
	_ = executeTelnyxHTTPCallback(node, state)
	if got := len(state.Variables["big"]); got != 1024 {
		t.Errorf("response not capped: got %d bytes, want 1024", got)
	}
}

func TestTelnyxIVRState_RoundTripWithVariables(t *testing.T) {
	original := &TelnyxIVRState{
		CurrentNode: "node_a",
		Path:        []string{"node_a"},
		Variables: map[string]string{
			"pin":                "1234",
			"__gather_store_as":  "pin",
			"customer_id":        "12345",
		},
	}
	encoded, err := EncodeTelnyxIVRState(original)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeTelnyxIVRState(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Variables) != 3 {
		t.Errorf("variables round-trip: got %d want 3", len(decoded.Variables))
	}
	for k, v := range original.Variables {
		if decoded.Variables[k] != v {
			t.Errorf("variable %q: got %q want %q", k, decoded.Variables[k], v)
		}
	}
}
