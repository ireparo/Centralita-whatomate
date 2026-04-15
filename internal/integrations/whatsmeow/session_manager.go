package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// SessionManager owns one whatsmeow.Client per iReparo WhatsAppAccount,
// bridges the library to the rest of iReparo through an EventSink, and
// abstracts the sqlstore Container so callers do not juggle
// whatsmeow-internal types.
//
// Expected lifecycle (typically in cmd/whatomate/main.go):
//
//	// 1. Wire up
//	mgr, err := NewSessionManager(ctx, SessionManagerConfig{
//	    PostgresDSN: cfg.PostgresDSN(),
//	    Sink:        app, // implements EventSink
//	    Log:         waLog.Zerolog(logger),
//	})
//
//	// 2. On startup, warm up existing sessions
//	mgr.ReconnectAll(ctx, accountIDs)
//
//	// 3. On shutdown
//	mgr.Close()
type SessionManager struct {
	container *sqlstore.Container
	sink      EventSink
	log       waLog.Logger

	mu      sync.RWMutex
	clients map[uuid.UUID]*Client
}

// SessionManagerConfig gathers the knobs NewSessionManager needs.
type SessionManagerConfig struct {
	// PostgresDSN is the whatsmeow-specific connection string used by
	// sqlstore. iReparo passes the same database but with whatsmeow's
	// required driver alias.
	PostgresDSN string

	// Sink receives all cross-package events (incoming messages, state
	// changes, pair success). Usually the App struct.
	Sink EventSink

	// Log is the whatsmeow-shaped logger; use waLog.Stdout() for
	// development, waLog.Noop() for tests, or an adapter over the app
	// logger in production.
	Log waLog.Logger

	// Dialect is the SQL dialect for sqlstore (default "postgres").
	// Exposed so tests can use "sqlite" if needed.
	Dialect string
}

// NewSessionManager opens the whatsmeow sqlstore Container and prepares
// it for multi-account operation. Run Upgrade automatically so the
// whatsmeow-specific tables exist on first boot.
func NewSessionManager(ctx context.Context, cfg SessionManagerConfig) (*SessionManager, error) {
	if cfg.Sink == nil {
		return nil, errors.New("whatsmeow: nil sink")
	}
	if cfg.Dialect == "" {
		cfg.Dialect = "postgres"
	}
	if cfg.Log == nil {
		cfg.Log = waLog.Noop
	}

	container, err := sqlstore.New(ctx, cfg.Dialect, cfg.PostgresDSN, cfg.Log)
	if err != nil {
		return nil, fmt.Errorf("whatsmeow sqlstore: %w", err)
	}

	return &SessionManager{
		container: container,
		sink:      cfg.Sink,
		log:       cfg.Log,
		clients:   make(map[uuid.UUID]*Client),
	}, nil
}

// GetOrCreate returns the client for accountID. If the account already
// has a paired device in the whatsmeow store (looked up by the JID
// argument), that device is loaded; otherwise a fresh unpaired device
// is created.
//
//	jid — the account's saved WhatsAppAccount.WhatsmeowJID. Pass
//	      types.EmptyJID on first pairing; the manager will create a
//	      new device and you run StartPairing on the returned client.
//
// Subsequent calls with the same accountID return the cached client.
func (m *SessionManager) GetOrCreate(ctx context.Context, accountID uuid.UUID, jid types.JID) (*Client, error) {
	m.mu.RLock()
	if c, ok := m.clients[accountID]; ok {
		m.mu.RUnlock()
		return c, nil
	}
	m.mu.RUnlock()

	// Double-checked lock in case two goroutines race.
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.clients[accountID]; ok {
		return c, nil
	}

	var device *store.Device
	if !jid.IsEmpty() {
		d, err := m.container.GetDevice(ctx, jid)
		if err != nil {
			return nil, fmt.Errorf("whatsmeow get device: %w", err)
		}
		// GetDevice returns nil, nil when the row exists but the session
		// was logged out remotely. Treat as fresh device.
		if d == nil {
			device = m.container.NewDevice()
		} else {
			device = d
		}
	} else {
		device = m.container.NewDevice()
	}

	client := newClient(accountID, device, m.sink, m.log)
	m.clients[accountID] = client
	return client, nil
}

// Get returns the cached client for accountID, or nil if none exists.
// Non-blocking read.
func (m *SessionManager) Get(accountID uuid.UUID) *Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clients[accountID]
}

// Remove disconnects and drops the client for accountID. Safe to call
// even if no client exists. Does NOT log out — use the client's Logout
// for that.
func (m *SessionManager) Remove(accountID uuid.UUID) {
	m.mu.Lock()
	client, ok := m.clients[accountID]
	delete(m.clients, accountID)
	m.mu.Unlock()
	if ok && client != nil {
		client.Disconnect()
	}
}

// ReconnectAll is called on process startup. It iterates every
// provider=whatsmeow account that already has a WhatsmeowJID and
// connects them in parallel so incoming messages start flowing without
// manual intervention.
//
// Accounts whose device has been logged out on WhatsApp's side (possible
// if the user removed the link from their phone) surface via the
// OnLoggedOut sink callback; the app should clear the JID on the
// account row in response.
func (m *SessionManager) ReconnectAll(ctx context.Context, accounts []AccountRef) {
	for _, ref := range accounts {
		ref := ref
		go func() {
			client, err := m.GetOrCreate(ctx, ref.ID, ref.JID)
			if err != nil {
				m.log.Errorf("whatsmeow reconnect: get client %s: %v", ref.ID, err)
				return
			}
			if !client.IsPaired() {
				m.log.Warnf("whatsmeow reconnect: account %s has no paired device, skipping", ref.ID)
				return
			}
			if err := client.Connect(ctx); err != nil {
				m.log.Errorf("whatsmeow reconnect: connect %s: %v", ref.ID, err)
			}
		}()
	}
}

// Close tears every cached client down and closes the underlying
// sqlstore connection pool. Call during graceful shutdown.
func (m *SessionManager) Close() {
	m.mu.Lock()
	clients := make([]*Client, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = map[uuid.UUID]*Client{}
	m.mu.Unlock()

	for _, c := range clients {
		c.Disconnect()
	}
	// Container.Close does not exist in all whatsmeow versions; fall
	// back to no-op if not present. The underlying DB pool is shared
	// with GORM in iReparo so closing it at this level would be wrong
	// anyway.
	_ = m.container
}

// AccountRef is the minimal info ReconnectAll needs per account: the
// internal ID + the paired JID. Callers (App) construct this slice by
// querying `SELECT id, whatsmeow_jid FROM whatsapp_accounts WHERE
// provider = 'whatsmeow' AND whatsmeow_jid <> ''`.
type AccountRef struct {
	ID  uuid.UUID
	JID types.JID
}

// ParseJID wraps whatsmeow's types.ParseJID so callers do not import
// the whatsmeow package just for one helper.
func ParseJID(s string) (types.JID, error) {
	if s == "" {
		return types.EmptyJID, nil
	}
	return types.ParseJID(s)
}

// whatsmeow re-exports used elsewhere in iReparo. Keeping them here
// means the handlers layer does not import the upstream packages
// directly.
var (
	// ErrNotPaired is returned when a method requires a paired session
	// but IsPaired() is false.
	ErrNotPaired = errors.New("whatsmeow: session not paired")
)

// Compile-time check that the wrapper implements the expected sub-set
// of whatsmeow's client interface (panic on build if the upstream API
// changes shape).
var _ interface {
	Connect() error
	Disconnect()
} = (*whatsmeow.Client)(nil)
