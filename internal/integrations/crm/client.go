package crm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration for the CRM integration. Loaded
// from the [integrations.crm] block of config.toml.
type Config struct {
	Enabled        bool
	BaseURL        string
	APIKey         string
	WebhookSecret  string
	LookupTimeout  time.Duration
	LookupCacheTTL time.Duration
	NegativeCacheTTL time.Duration
	HTTPTimeout    time.Duration
	UserAgent      string
}

// Defaults applies sensible defaults for any zero-valued field.
func (c *Config) Defaults() {
	if c.LookupTimeout <= 0 {
		c.LookupTimeout = 1500 * time.Millisecond
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 5 * time.Second
	}
	if c.LookupCacheTTL <= 0 {
		c.LookupCacheTTL = 5 * time.Minute
	}
	if c.NegativeCacheTTL <= 0 {
		c.NegativeCacheTTL = 30 * time.Second
	}
	if c.UserAgent == "" {
		c.UserAgent = "iReparo-PBX/1.0"
	}
}

// Client is the iReparo PBX → CRM HTTP client. It is safe for concurrent
// use by multiple goroutines.
type Client struct {
	cfg        Config
	httpClient *http.Client
	cache      *LookupCache
}

// NewClient builds a CRM client. Pass an httpClient to share connection
// pools with the rest of iReparo (recommended); pass nil and one will
// be created.
func NewClient(cfg Config, httpClient *http.Client) *Client {
	cfg.Defaults()
	if httpClient == nil {
		httpClient = &http.Client{Timeout: cfg.HTTPTimeout}
	}
	return &Client{
		cfg:        cfg,
		httpClient: httpClient,
		cache:      NewLookupCache(cfg.LookupCacheTTL, cfg.NegativeCacheTTL),
	}
}

// Enabled reports whether the CRM integration is configured. Callers should
// check this before invoking Lookup/Send.
func (c *Client) Enabled() bool {
	return c != nil && c.cfg.Enabled && c.cfg.BaseURL != ""
}

// BaseURL returns the configured base URL. Used for building screen-pop
// links in the agent panel.
func (c *Client) BaseURL() string {
	if c == nil {
		return ""
	}
	return strings.TrimRight(c.cfg.BaseURL, "/")
}

// Lookup queries GET /api/pbx/lookup?phone=<normalized> with a tight
// timeout, returning the customer info if found.
//
// The cache is consulted first; on a hit, the network call is skipped.
// On a network error or timeout, the function returns (nil, err) and
// the caller should fall back to "unknown caller" UX.
//
// IMPORTANT: callers should NOT block UI rendering on this — wrap the
// call in a context with a tight timeout (e.g. 1.5s) and proceed with
// "unknown" if it does not return in time.
func (c *Client) Lookup(ctx context.Context, phoneRaw string) (*LookupResponse, error) {
	if !c.Enabled() {
		return nil, errors.New("crm: integration disabled")
	}
	phone := NormalizePhone(phoneRaw)
	if phone == "" {
		return nil, errors.New("crm: empty phone")
	}

	if cached, ok := c.cache.Get(phone); ok {
		return cached, nil
	}

	url := c.BaseURL() + "/api/pbx/lookup?phone=" + phone
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("crm lookup: build request: %w", err)
	}
	req.Header.Set(HeaderAPIKey, c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	// Firma HMAC-SHA256 sobre "{timestamp}." (body vacío) para que el
	// CRM acepte el GET — su authenticate() exige timestamp + firma en
	// TODAS las llamadas (defensa en profundidad contra fuga del API
	// key suelta). Antes de este fix, todo lookup contra el CRM
	// configurado respondería 401.
	if c.cfg.WebhookSecret != "" {
		ts := time.Now().Unix()
		sig := SignPayload(c.cfg.WebhookSecret, ts, nil)
		req.Header.Set(HeaderSignature, sig)
		req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("crm lookup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB safety cap
	if err != nil {
		return nil, fmt.Errorf("crm lookup: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("crm lookup: http %d: %s", resp.StatusCode, truncate(body, 200))
	}

	var out LookupResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("crm lookup: unmarshal: %w", err)
	}
	if out.NormalizedPhone == "" {
		out.NormalizedPhone = phone
	}
	c.cache.Put(phone, &out)
	return &out, nil
}

// InvalidateCache removes a phone from the lookup cache. Call this after
// a manual link operation in the iReparo UI so the next call gets fresh
// data immediately.
func (c *Client) InvalidateCache(phone string) {
	if c == nil {
		return
	}
	c.cache.Invalidate(NormalizePhone(phone))
}

// EventEnvelope is what BuildEvent returns: a ready-to-send envelope plus
// the precomputed signature header. Caller can either send it immediately
// (Send) or persist it to the retry queue (Enqueue) and let the worker
// pick it up.
type EventEnvelope struct {
	EventType string
	Body      []byte
	Signature string
	Timestamp int64
	URL       string
}

// BuildEvent serializes an event payload into an envelope (JSON body) and
// computes the HMAC signature. The returned EventEnvelope is suitable for
// either immediate sending or persisting to the retry queue.
func (c *Client) BuildEvent(eventType string, data any) (*EventEnvelope, error) {
	if !c.Enabled() {
		return nil, errors.New("crm: integration disabled")
	}
	env := Envelope{
		Event:     eventType,
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
	body, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("crm build event: marshal: %w", err)
	}
	ts := env.Timestamp.Unix()
	sig := SignPayload(c.cfg.WebhookSecret, ts, body)
	return &EventEnvelope{
		EventType: eventType,
		Body:      body,
		Signature: sig,
		Timestamp: ts,
		URL:       c.BaseURL() + "/api/pbx/call-event",
	}, nil
}

// Send POSTs an EventEnvelope to the CRM. Returns nil on 2xx, an error
// otherwise. The caller decides whether to enqueue for retry on error.
func (c *Client) Send(ctx context.Context, env *EventEnvelope) error {
	if !c.Enabled() {
		return errors.New("crm: integration disabled")
	}
	if env == nil {
		return errors.New("crm: nil envelope")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, env.URL, bytes.NewReader(env.Body))
	if err != nil {
		return fmt.Errorf("crm send: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set(HeaderAPIKey, c.cfg.APIKey)
	req.Header.Set(HeaderSignature, env.Signature)
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(env.Timestamp, 10))
	req.Header.Set(HeaderEventType, env.EventType)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("crm send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return fmt.Errorf("crm send: http %d: %s", resp.StatusCode, truncate(body, 200))
	}
	// Drain the body so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "..."
	}
	return string(b)
}
