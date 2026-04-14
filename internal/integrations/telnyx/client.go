// Package telnyx provides a thin wrapper around the Telnyx Call Control API
// (v2) that iReparo uses to handle PSTN calls (regular phone numbers, both
// fixed and mobile) in parallel to the existing WhatsApp Cloud calling.
//
// The package is intentionally minimal — it only exposes the operations the
// rest of iReparo needs:
//
//   - Initiating outbound calls (Dial)
//   - Answering, hanging up, transferring (Answer, Hangup, Transfer)
//   - Playing audio + DTMF gathering (used by the existing IVR engine)
//   - Querying account/number metadata
//
// All HTTP calls go through a single net/http client with sane timeouts and
// retries on transient 5xx. Authentication is per-call, since each
// organization brings its own Telnyx API key.
package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BaseURL is the production Telnyx API endpoint. Telnyx does not have a
// public sandbox; all calls go to production and are billed accordingly.
const BaseURL = "https://api.telnyx.com/v2"

// DefaultTimeout for one-off HTTP requests to the Telnyx API. Real call
// commands (answer, hangup, etc.) typically respond in <500 ms.
const DefaultTimeout = 10 * time.Second

// Client is a per-request handle to the Telnyx API. It is cheap to construct
// (no internal state besides http.Client and the API key), so callers should
// build one per organization or per request rather than caching it globally.
type Client struct {
	apiKey     string
	httpClient *http.Client
	baseURL    string
}

// NewClient builds a Telnyx API client for a given organization. The apiKey
// must be the v2 key (it starts with "KEY"). If httpClient is nil, a default
// one with sane timeouts is created.
func NewClient(apiKey string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		apiKey:     apiKey,
		httpClient: httpClient,
		baseURL:    BaseURL,
	}
}

// WithBaseURL overrides the default Telnyx base URL. Used by tests to point
// at a local mock server.
func (c *Client) WithBaseURL(url string) *Client {
	c.baseURL = url
	return c
}

// APIError represents a structured error returned by the Telnyx API.
// Telnyx returns errors as a JSON object with an "errors" array; we keep
// the most relevant fields.
type APIError struct {
	StatusCode int    `json:"-"`
	Code       string `json:"code"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("telnyx: %s (%d): %s", e.Title, e.StatusCode, e.Detail)
	}
	return fmt.Sprintf("telnyx: %s (%d)", e.Title, e.StatusCode)
}

// do executes an HTTP request against the Telnyx API and unmarshals a JSON
// response body into out (if non-nil). It handles 4xx/5xx by returning an
// APIError with the status code attached.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("telnyx: marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("telnyx: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telnyx: do request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("telnyx: read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		// Try to decode the error envelope. Telnyx wraps errors in
		// {"errors": [{"code":"...","title":"...","detail":"..."}]}
		var envelope struct {
			Errors []APIError `json:"errors"`
		}
		_ = json.Unmarshal(respBytes, &envelope)
		apiErr := &APIError{StatusCode: resp.StatusCode}
		if len(envelope.Errors) > 0 {
			*apiErr = envelope.Errors[0]
			apiErr.StatusCode = resp.StatusCode
		} else {
			apiErr.Title = "unknown error"
			apiErr.Detail = string(respBytes)
		}
		return apiErr
	}

	if out != nil && len(respBytes) > 0 {
		if err := json.Unmarshal(respBytes, out); err != nil {
			return fmt.Errorf("telnyx: unmarshal response: %w", err)
		}
	}
	return nil
}

// --- Account / connectivity check ------------------------------------------

// Whoami calls the /me endpoint to verify the API key is valid. Used during
// onboarding (when an admin first pastes credentials in the iReparo UI) and
// periodically to detect rotated keys.
func (c *Client) Whoami(ctx context.Context) (*WhoamiResponse, error) {
	var resp struct {
		Data WhoamiResponse `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/me", nil, &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// WhoamiResponse mirrors the relevant fields of GET /v2/me.
type WhoamiResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	UserName  string `json:"user_name"`
	CompanyID string `json:"company_id"`
}

// --- Outbound call ----------------------------------------------------------

// DialRequest is the payload for POST /v2/calls.
//
// Required fields: To, From, ConnectionID. The ConnectionID is the Telnyx
// Call Control Application ID configured in the Telnyx panel and stored in
// TelnyxConnection.CallControlAppID.
type DialRequest struct {
	To           string `json:"to"`
	From         string `json:"from"`
	ConnectionID string `json:"connection_id"`

	// Optional. If set, Telnyx will POST events about this call to the
	// connection's webhook URL. Override per-call if you want a different
	// webhook than the connection default.
	WebhookURL string `json:"webhook_url,omitempty"`

	// Optional. ClientState is an opaque base64 string echoed back in every
	// webhook event for this call. iReparo uses it to correlate events to
	// the in-flight CallLog row.
	ClientState string `json:"client_state,omitempty"`

	// AnsweringMachineDetection: "premium", "detect", "detect_words" or "".
	// Use "premium" for outbound campaigns that should hang up if a machine
	// answers.
	AnsweringMachineDetection string `json:"answering_machine_detection,omitempty"`

	// Record: "record-from-answer" enables call recording from the moment
	// the callee picks up.
	Record string `json:"record,omitempty"`
}

// DialResponse is the relevant subset of POST /v2/calls.
type DialResponse struct {
	CallControlID  string `json:"call_control_id"`
	CallLegID      string `json:"call_leg_id"`
	CallSessionID  string `json:"call_session_id"`
	IsAlive        bool   `json:"is_alive"`
	RecordEnabled  bool   `json:"record_enabled"`
	ClientState    string `json:"client_state"`
}

// Dial places an outbound call. Returns the Call Control ID needed for any
// subsequent action on this call (Answer, Hangup, etc.).
func (c *Client) Dial(ctx context.Context, req *DialRequest) (*DialResponse, error) {
	var resp struct {
		Data DialResponse `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/calls", req, &resp); err != nil {
		return nil, err
	}
	return &resp.Data, nil
}

// --- Answer / Hangup --------------------------------------------------------

// Answer accepts an inbound ringing call. Used when iReparo wants to bridge
// the call into the IVR rather than letting it auto-answer.
func (c *Client) Answer(ctx context.Context, callControlID string) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/answer", callControlID),
		map[string]any{}, nil)
}

// Hangup terminates a call. Idempotent: calling on an already-ended call
// returns 422, which we treat as a no-op.
func (c *Client) Hangup(ctx context.Context, callControlID string) error {
	err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/hangup", callControlID),
		map[string]any{}, nil)
	if err != nil {
		var apiErr *APIError
		if asAPIErr(err, &apiErr) && apiErr.StatusCode == http.StatusUnprocessableEntity {
			return nil // already ended
		}
	}
	return err
}

// --- Transfer ---------------------------------------------------------------

// TransferRequest moves an active call to another destination (another phone
// number or another Telnyx connection).
type TransferRequest struct {
	To          string `json:"to"`
	From        string `json:"from,omitempty"`
	ClientState string `json:"client_state,omitempty"`
}

// Transfer moves an in-progress call to another destination.
func (c *Client) Transfer(ctx context.Context, callControlID string, req *TransferRequest) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/transfer", callControlID),
		req, nil)
}

// --- Audio / DTMF (used by the IVR engine) ---------------------------------

// PlaybackRequest tells Telnyx to stream a media file to the call.
type PlaybackRequest struct {
	AudioURL    string `json:"audio_url"`
	Loop        int    `json:"loop,omitempty"`
	ClientState string `json:"client_state,omitempty"`
}

// PlayAudio streams an audio file (WAV/MP3 over HTTPS) to the call.
func (c *Client) PlayAudio(ctx context.Context, callControlID string, req *PlaybackRequest) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/playback_start", callControlID),
		req, nil)
}

// GatherRequest gathers DTMF input from the caller for the IVR.
type GatherRequest struct {
	MinDigits        int    `json:"minimum_digits,omitempty"`
	MaxDigits        int    `json:"maximum_digits,omitempty"`
	TerminatingDigit string `json:"terminating_digit,omitempty"`
	TimeoutMillis    int    `json:"timeout_millis,omitempty"`
	ValidDigits      string `json:"valid_digits,omitempty"`
	ClientState      string `json:"client_state,omitempty"`
}

// Gather starts collecting DTMF tones from the caller.
func (c *Client) Gather(ctx context.Context, callControlID string, req *GatherRequest) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/gather", callControlID),
		req, nil)
}

// GatherUsingAudioRequest plays an audio prompt and gathers DTMF input in a
// single action. This is the canonical Telnyx pattern for an IVR menu — it
// is atomic (playback + gather in one command) and interruptible (caller
// pressing a digit stops the audio and the digit counts towards the gather).
type GatherUsingAudioRequest struct {
	AudioURL         string `json:"audio_url"`
	MinDigits        int    `json:"minimum_digits,omitempty"`
	MaxDigits        int    `json:"maximum_digits,omitempty"`
	TerminatingDigit string `json:"terminating_digit,omitempty"`
	TimeoutMillis    int    `json:"timeout_millis,omitempty"`
	ValidDigits      string `json:"valid_digits,omitempty"`
	InvalidAudioURL  string `json:"invalid_audio_url,omitempty"`
	ClientState      string `json:"client_state,omitempty"`
}

// GatherUsingAudio plays an audio file and gathers DTMF input in one action.
// Telnyx emits a call.gather.ended event when the caller finishes (or times
// out), carrying the digits they pressed and an echo of ClientState.
func (c *Client) GatherUsingAudio(ctx context.Context, callControlID string, req *GatherUsingAudioRequest) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/gather_using_audio", callControlID),
		req, nil)
}

// --- Recording --------------------------------------------------------------

// RecordRequest starts recording an in-progress call.
type RecordRequest struct {
	Format      string `json:"format,omitempty"`  // "mp3" or "wav"
	Channels    string `json:"channels,omitempty"` // "single" or "dual"
	ClientState string `json:"client_state,omitempty"`
}

// StartRecording asks Telnyx to begin recording a call. The recording URL
// arrives later in a "call.recording.saved" webhook event.
func (c *Client) StartRecording(ctx context.Context, callControlID string, req *RecordRequest) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/record_start", callControlID),
		req, nil)
}

// StopRecording stops an in-progress recording before the call ends.
func (c *Client) StopRecording(ctx context.Context, callControlID string) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/calls/%s/actions/record_stop", callControlID),
		map[string]any{}, nil)
}

// --- internal helpers -------------------------------------------------------

// asAPIErr is a small helper that unwraps an *APIError from the error chain
// without requiring callers to import errors and do the type-assertion dance.
func asAPIErr(err error, target **APIError) bool {
	if err == nil {
		return false
	}
	if apiErr, ok := err.(*APIError); ok {
		*target = apiErr
		return true
	}
	return false
}
