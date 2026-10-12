package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/auth"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
	"github.com/Skryldev/suromesh/transport"
	"github.com/Skryldev/suromesh/transport/ws"
)

// Config configures a Client. Defaults mirror the reference client where
// protocol-relevant (device strings, layer).
type Config struct {
	// Session carries the DC routing and auth key (required). For a
	// session with persistence, wrap it: Saver is invoked after key
	// exchange so FileSession users persist automatically.
	Session *session.Session
	// Saver persists the session after state changes (e.g. FileSession).
	Saver interface{ Save() error }

	// Host overrides the session's DC address ("host:port"). Default:
	// im-server.splus.ir:443 over TLS (the Soroush endpoint).
	Host string
	// TLSEnabled selects wss vs ws (default true).
	TLSEnabled *bool

	// Device identification (reference defaults shown).
	APIID          int32
	APIHash        string
	DeviceModel    string
	SystemVersion  string
	AppVersion     string
	LangCode       string
	SystemLangCode string

	// RequestTimeout bounds an Invoke whose context has no deadline
	// (default 30 s; negative disables).
	RequestTimeout time.Duration

	// Retries bounds automatic re-invocation for retryable failures
	// (server errors, tolerated flood waits) — default 5, reference
	// parity. Non-idempotent ambiguity (connection loss mid-flight) is
	// NEVER retried automatically.
	Retries int

	// FloodWaitThreshold: flood waits not above this are slept through
	// and the request retried (server did not execute it — safe).
	// Default 60 s (reference flood_sleep_threshold).
	FloodWaitThreshold time.Duration

	// RetryOnReconnect replays pending requests after a transport
	// reconnect (reference behavior). Unsafe for non-idempotent
	// operations; default false — pending requests fail with
	// rpc.ErrConnectionLost instead.
	RetryOnReconnect bool

	// MaxPending bounds concurrently in-flight RPCs (default 8192).
	// Invokes beyond the bound fail with rpc.ErrPendingLimit before any
	// bytes are sent.
	MaxPending int

	// EntityCacheSize bounds the entity cache (default 4096). Capacity
	// gates NEW keys only; updates to known peers always apply.
	EntityCacheSize int

	// ConversationCacheSize bounds the derived conversation store
	// (default 1024). Eviction drops derived state only — protocol
	// layers are unaffected.
	ConversationCacheSize int

	// Transfer configures the media/file subsystem (workers, concurrency,
	// progress interval). Zero values use defaults.
	Transfer TransferConfig

	// UpdateWorkers is the update handler pool size (default 4).
	// UpdateQueue bounds the update work queue (default 1024).
	UpdateWorkers int
	UpdateQueue   int

	Hooks Hooks
}

func (c *Config) fill() {
	if c.Session == nil {
		c.Session = session.New()
	}
	if c.Host == "" {
		if _, addr, port := c.Session.DC(); addr != "" {
			c.Host = fmt.Sprintf("%s:%d", addr, port)
		} else {
			c.Host = "im-server.splus.ir:443"
		}
	}
	if c.APIID == 0 {
		c.APIID = 1030400 // reference default
	}
	if c.APIHash == "" {
		c.APIHash = "6edb16cf88714a4e9a805e928c39c937"
	}
	if c.DeviceModel == "" {
		c.DeviceModel = "PC 64bit"
	}
	if c.SystemVersion == "" {
		c.SystemVersion = "go"
	}
	if c.AppVersion == "" {
		c.AppVersion = "suromesh"
	}
	if c.LangCode == "" {
		c.LangCode = "en"
	}
	if c.SystemLangCode == "" {
		c.SystemLangCode = "en"
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 30 * time.Second
	}
	if c.Retries == 0 {
		c.Retries = 5
	}
	if c.FloodWaitThreshold == 0 {
		c.FloodWaitThreshold = 60 * time.Second
	}
	if c.UpdateWorkers == 0 {
		c.UpdateWorkers = 4
	}
	if c.UpdateQueue == 0 {
		c.UpdateQueue = 1024
	}
}

// Client is the SuroMesh user-facing client. Safe for concurrent use.
type Client struct {
	cfg  Config
	sess *session.Session

	sm stateMachine

	mu  sync.RWMutex
	tr  *transport.Transport
	eng *rpc.Engine

	entities      *entityCache
	updates       *updateManager
	transfers     *transferManager
	conversations *conversationStore

	// Authorization state. authHashes/authPhone mirror the reference's
	// _phone_code_hash / _phone (in-memory only, never persisted);
	// authFlow is the transient pending-code/password marker; authRevoked
	// is set by the permanent 401 set. Guarded per comments in auth.go.
	authMu      sync.Mutex // guards authHashes/authPhone
	authHashes  map[string]string
	authPhone   string
	authFlow    atomic.Int32 // authFlow (auth.go)
	authRevoked atomic.Bool
	authMetrics authMetrics

	lastErr atomic.Value // error: terminal connect failure

	selfID int64 // atomic: authenticated user id (InputPeerSelf rule)

	connectMu sync.Mutex  // serializes Connect/Disconnect/Close bodies
	closed    atomic.Bool // terminal flag; read on every Invoke/OnUpdate

	lifecycleCtx    context.Context // cancelled by Close; interrupts Connect, Invoke, retry sleeps
	lifecycleCancel context.CancelFunc
}

// New creates a client in StateNew. Connect starts it.
func New(cfg Config) (*Client, error) {
	cfg.fill()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	c := &Client{
		cfg:           cfg,
		sess:          cfg.Session,
		entities:      newEntityCache(cfg.entityCacheSizeOrDefault()),
		conversations: newConversationStore(cfg.ConversationCacheSize),
		authHashes:    make(map[string]string),
	}
	c.lifecycleCtx, c.lifecycleCancel = context.WithCancel(context.Background())
	tm, terr := newTransferManager(cfg.Transfer)
	if terr != nil {
		return nil, fmt.Errorf("client: transfer config: %w", terr)
	}
	c.transfers = tm
	c.sm.transition(StateNew, StateDisconnected)
	c.sm.subscribe(func(from, to State) {
		if c.cfg.Hooks.OnStateChange != nil {
			c.cfg.Hooks.OnStateChange(from, to)
		}
	})
	return c, nil
}

// State returns the lifecycle state.
func (c *Client) State() State { return c.sm.get() }

// IsConnected reports whether the client is Ready and the transport is up.
func (c *Client) IsConnected() bool {
	if c.sm.get() != StateReady {
		return false
	}
	c.mu.RLock()
	tr := c.tr
	c.mu.RUnlock()
	return tr != nil && (tr.State() == transport.StateConnected || tr.State() == transport.StateReconnecting)
}

// Connect brings the client to Ready: transport dial → auth exchange when
// no key exists → engine start → protocol initialization
// (invokeWithLayer+initConnection, reference-compatible tolerance).
// Concurrent Connect calls serialize; a failed Connect leaves the client
// deterministically in Disconnected with the error retrievable via
// LastError. Context cancellation terminates the attempt.
func (c *Client) Connect(ctx context.Context) error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}

	switch c.sm.get() {
	case StateReady, StateConnected:
		return nil // idempotent
	case StateConnecting:
		return ErrConnectInProgress
	case StateDisconnected, StateNew:
		// proceed
	default:
		return fmt.Errorf("%w: connect from %s", ErrBadState, c.sm.get())
	}
	if !c.sm.transition(c.sm.get(), StateConnecting) {
		return fmt.Errorf("%w: connect race", ErrBadState)
	}
	// Close must interrupt an in-flight Connect: derive the operation
	// context from the caller's ctx AND the lifecycle context.
	opCtx, cancelOp := mergeContext(ctx, c.lifecycleCtx)
	defer cancelOp()

	if err := c.connectLocked(opCtx); err != nil {
		// Deterministic failure state: fully unwound.
		c.teardownLocked()
		c.sm.transition(StateConnecting, StateDisconnected)
		c.lastErr.Store(err)
		return err
	}
	c.sm.transition(StateConnected, StateReady)
	return nil
}

func (c *Client) connectLocked(opCtx context.Context) error {
	tlsEnabled := true
	if c.cfg.TLSEnabled != nil {
		tlsEnabled = *c.cfg.TLSEnabled
	}
	tr := transport.New(transport.Config{
		Host:             c.cfg.Host,
		WS:               ws.Options{TLSEnabled: tlsEnabled},
		PingInterval:     30 * time.Second,
		MaxConnectionAge: 30 * time.Minute,
	})
	// Transport-level terminal failure must unwind Connect deterministically.
	termErr := make(chan error, 1)
	tr.Subscribe(func(from, to transport.State) {
		if to == transport.StateClosed && c.sm.get() == StateConnecting {
			select {
			case termErr <- c.transportTerminalError():
			default:
			}
		}
	})
	if err := tr.Connect(opCtx); err != nil {
		tr.Close()
		return fmt.Errorf("client: transport: %w", err)
	}

	// Auth: reuse the persisted key; exchange only when absent. A
	// reconnect (this method re-entered after Disconnect) never re-runs
	// the exchange while the session key is valid.
	if err := opCtx.Err(); err != nil {
		tr.Close()
		return fmt.Errorf("client: cancelled during connect: %w", err)
	}

	key := c.sess.AuthKey()
	if key == nil || !key.HasValue {
		res, err := auth.Exchange(opCtx, auth.TransportPlainSender{Tr: tr}, auth.Config{})
		if err != nil {
			tr.Close()
			return fmt.Errorf("client: auth exchange: %w", err)
		}
		if err := c.sess.SetAuthKey(res.AuthKey); err != nil {
			tr.Close()
			return fmt.Errorf("client: session: %w", err)
		}
		if c.cfg.Saver != nil {
			if err := c.cfg.Saver.Save(); err != nil {
				tr.Close()
				return fmt.Errorf("client: session save: %w", err)
			}
		}
		if c.cfg.Hooks.OnAuthenticated != nil {
			c.cfg.Hooks.OnAuthenticated()
		}
	} else if c.cfg.Hooks.OnAuthenticated != nil {
		c.cfg.Hooks.OnAuthenticated()
	}

	if err := opCtx.Err(); err != nil {
		tr.Close()
		return fmt.Errorf("client: cancelled during connect: %w", err)
	}

	eng, err := rpc.New(tr, rpc.Config{
		AuthKey:          c.sess.AuthKey(),
		DefaultTimeout:   c.cfg.RequestTimeout,
		RetryOnReconnect: c.cfg.RetryOnReconnect,
		MaxPending:       c.cfg.MaxPending,
	})
	if err != nil {
		tr.Close()
		return fmt.Errorf("client: engine: %w", err)
	}

	c.mu.Lock()
	c.tr = tr
	c.eng = eng
	c.mu.Unlock()
	eng.Start()

	c.updates = newUpdateManager(c.cfg.UpdateWorkers, c.cfg.UpdateQueue, c.cfg.Hooks)
	c.updates.client = c
	c.updates.persist = c.persistUpdateState
	// Reconnect synchronization (§17): load the persisted update state so
	// the pts frontier survives restarts — missed updates then surface as
	// ordinary gaps and are recovered through the normal difference flow.
	c.seedUpdateStateFromSession()
	c.updates.start(eng.Updates(), eng)

	// Track transport death while Ready: surface as Disconnected (reconnect
	// is possible by calling Connect again).
	tr.Subscribe(func(from, to transport.State) {
		if to == transport.StateClosed && c.sm.get() == StateReady {
			c.sm.transitionAnyTo(StateClosing)
			c.sm.transition(StateClosing, StateDisconnected)
		}
	})

	if !c.sm.transition(StateConnecting, StateConnected) {
		c.teardownLocked()
		return fmt.Errorf("%w: state race during connect", ErrBadState)
	}

	// Protocol initialization (reference connect(): the first query is
	// invokeWithLayer(initConnection(help.getConfig)); Soroush answers
	// config with custom constructor IDs, so decode failures are
	// tolerated exactly like the reference ignores TypeNotFoundError).
	if err := c.initialize(opCtx); err != nil {
		c.teardownLocked()
		return err
	}
	return nil
}

// initialize performs invokeWithLayer(182, initConnection(..., getConfig)).
func (c *Client) initialize(ctx context.Context) error {
	req := &gen.InvokeWithLayerRequest{
		Layer: gen.Layer,
		Query: &gen.InitConnectionRequest{
			APIID:          c.cfg.APIID,
			DeviceModel:    c.cfg.DeviceModel,
			SystemVersion:  c.cfg.SystemVersion,
			AppVersion:     c.cfg.AppVersion,
			SystemLangCode: c.cfg.SystemLangCode,
			LangPack:       "",
			LangCode:       c.cfg.LangCode,
			Query:          &gen.HelpGetConfigRequest{},
		},
	}
	_, err := c.eng.Invoke(ctx, req)
	if err != nil {
		if isToleratedInitError(err) {
			return nil // reference-compatible: custom config ctors
		}
		return fmt.Errorf("client: initialize: %w", err)
	}
	return nil
}

// isToleratedInitError mirrors the reference's connect-time tolerance
// (TypeNotFoundError / BufferError on the config response).
func isToleratedInitError(err error) bool {
	for e := err; e != nil; {
		if errors.Is(e, tl.ErrUnknownConstructor) ||
			errors.Is(e, tl.ErrUnexpectedEOF) ||
			errors.Is(e, tl.ErrInvalidLength) ||
			errors.Is(e, tl.ErrConstructorMismatch) {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// Disconnect gracefully stops the client (state → Disconnected); Connect
// may be called again. Pending requests fail with rpc.ErrEngineClosed.
func (c *Client) Disconnect(ctx context.Context) error {
	if c.closed.Load() {
		return ErrClosed
	}
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	switch c.sm.get() {
	case StateNew, StateDisconnected, StateClosed:
		return nil
	}
	c.sm.transitionAnyTo(StateClosing)
	c.teardownLocked()
	c.sm.transition(StateClosing, StateDisconnected)
	return nil
}

// Close is final: stops everything and marks the client Closed. Safe to
// call multiple times and concurrent with Connect.
func (c *Client) Close() error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}
	c.closed.Store(true)
	// Interrupt everything derived from the lifecycle context FIRST so an
	// in-flight Connect (which holds connectMu via the caller of Close? no —
	// Connect holds it; we blocked on it) unwinds as soon as it reacquires
	// the mutex. Connect phases observe the cancellation between steps and
	// through the merged operation context.
	c.lifecycleCancel()
	c.sm.transitionAnyTo(StateClosing)
	c.teardownLocked()
	c.sm.transition(StateClosing, StateClosed)
	return nil
}

// teardownLocked stops engine, updates, and transport (connectMu held).
func (c *Client) teardownLocked() {
	// Nil out shared pointers under c.mu (the same lock readers use) so
	// OnUpdate/Invoke never see a half-torn-down client.
	c.mu.Lock()
	eng := c.eng
	tr := c.tr
	upd := c.updates
	c.eng = nil
	c.tr = nil
	c.updates = nil
	c.mu.Unlock()
	if upd != nil {
		upd.stop()
	}
	if eng != nil {
		eng.Close()
	}
	if tr != nil {
		tr.Close()
	}
	if c.transfers != nil {
		c.transfers.stop()
	}
}

// ClosedForTest reports the closed flag (test seam for the external
// test package).
func (c *Client) ClosedForTest() bool { return c.closed.Load() }

// SessionKeyID returns the current auth key id (0 = none). Test seam for
// reconnect-invariant assertions.
func (c *Client) SessionKeyID() int64 {
	k := c.sess.AuthKey()
	if k == nil {
		return 0
	}
	return int64(k.KeyID)
}

// ReconnectTransportForTest forces a transport connection loss (test
// seam for reconnect-invariant tests).
func (c *Client) ReconnectTransportForTest() {
	c.mu.RLock()
	tr := c.tr
	c.mu.RUnlock()
	if tr != nil {
		tr.ForceReconnectForTest()
	}
}

// logoutTeardown unwinds the transport, engine, and update manager after
// a successful logout (reference auth.py:617 disconnects). The transfer
// manager is client-level state and survives; the lifecycle returns to
// Disconnected so a subsequent Connect re-establishes everything (with a
// keyless session, a fresh key exchange — the reference deletes the
// session outright; the Go client stays a usable object).
func (c *Client) logoutTeardown() {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()
	c.mu.Lock()
	eng, tr, upd := c.eng, c.tr, c.updates
	c.eng, c.tr, c.updates = nil, nil, nil
	c.mu.Unlock()
	if upd != nil {
		upd.stop()
	}
	if eng != nil {
		eng.Close()
	}
	if tr != nil {
		tr.Close()
	}
	c.sm.transitionAnyTo(StateClosing)
	c.sm.transition(StateClosing, StateDisconnected)
}

// LastError returns the terminal error of the most recent failed Connect.
func (c *Client) LastError() error {
	err, _ := c.lastErr.Load().(error)
	return err
}

func (c *Client) transportTerminalError() error {
	c.mu.RLock()
	tr := c.tr
	c.mu.RUnlock()
	if tr != nil {
		if err := tr.TerminalErr(); err != nil {
			return fmt.Errorf("client: transport failed: %w", err)
		}
	}
	return errors.New("client: transport closed")
}

// engine returns the live engine or an error when not running.
func (c *Client) engine() (*rpc.Engine, error) {
	if c.closed.Load() {
		return nil, ErrClosed
	}
	if s := c.sm.get(); s != StateReady && s != StateConnected {
		if s == StateDisconnected && c.lastErr.Load() != nil {
			return nil, fmt.Errorf("client: disconnected after failure: %w", c.LastError())
		}
		return nil, ErrNotReady
	}
	c.mu.RLock()
	eng := c.eng
	c.mu.RUnlock()
	if eng == nil {
		return nil, ErrNotReady
	}
	return eng, nil
}
