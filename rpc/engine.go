package rpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/transport"
)

// Config tunes the engine. Defaults mirror the reference where the
// protocol dictates them (timeouts, resend-on-bad-salt) and choose the
// conservative side where it does not (no automatic retransmission after
// connection loss).
type Config struct {
	// AuthKey is the session's authorization key (Phase 5 will manage its
	// lifecycle; the engine requires one up front).
	AuthKey *crypto.AuthKey

	// DefaultTimeout bounds an Invoke whose context carries no deadline
	// (default 30 s; 0 uses the default, negative disables).
	DefaultTimeout time.Duration

	// MaxPending bounds concurrently in-flight requests (default 8192).
	// Exceeding it fails the Invoke with ErrPendingLimit before anything
	// is sent — a deterministic, retry-safe rejection that protects the
	// registry from unbounded growth when the server stalls.
	MaxPending int

	// UpdatesQueue bounds the updates channel (default 1024). Overflow
	// drops the newest update with a counter — updates never block the
	// response dispatcher.
	UpdatesQueue int

	// ResendOnBadSalt re-sends a request whose message the server rejected
	// with bad_server_salt after adopting the new salt. The reference does
	// this; the request provably never executed. Default true.
	ResendOnBadSalt bool

	// RetryOnReconnect replays pending requests after a connection loss
	// (reference behavior, re-registered under fresh message IDs). This is
	// semantically unsafe for non-idempotent operations; the default false
	// fails pending requests with ErrConnectionLost instead.
	RetryOnReconnect bool

	// Rand supplies envelope padding (default crypto/rand).
	Rand interface {
		Read(p []byte) (int, error)
	}

	// Now allows tests to pin the clock used for time offsets.
	Now func() time.Time
}

func (c *Config) fill() {
	if c.DefaultTimeout == 0 {
		c.DefaultTimeout = 30 * time.Second
	}
	if c.UpdatesQueue <= 0 {
		c.UpdatesQueue = 1024
	}
	if c.MaxPending == 0 {
		c.MaxPending = 8192
	}
	if c.Rand == nil {
		c.Rand = mtproto.Rand
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Engine executes MTProto RPC requests over a Phase 3 transport.
//
// Concurrency (docs/concurrency.md): one response-dispatcher goroutine owns
// inbound correlation; callers invoke Invoke from arbitrary goroutines; the
// registry is the only shared mutable structure (RWMutex map). No
// goroutine-per-request ever exists.
type Engine struct {
	cfg Config
	tr  *transport.Transport

	metrics Metrics
	reg     *registry
	ids     *idAssigner

	salt      atomic.Int64
	sessionID atomic.Int64

	updates chan tl.Object

	replay       []*pending
	replayMu     sync.Mutex
	replaySignal chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	closeMu sync.Mutex
	closed  bool
}

// New attaches an engine to a connected (or about-to-connect) transport.
// The engine starts its dispatcher when Start is called.
func New(tr *transport.Transport, cfg Config) (*Engine, error) {
	cfg.fill()
	if cfg.AuthKey == nil || !cfg.AuthKey.HasValue {
		return nil, fmt.Errorf("rpc: engine requires an auth key")
	}
	e := &Engine{
		cfg:     cfg,
		tr:      tr,
		ids:     newIDAssigner(0),
		updates: make(chan tl.Object, cfg.UpdatesQueue),
		done:    make(chan struct{}),
	}
	e.reg = newRegistry(&e.metrics)
	e.reg.setLimit(cfg.MaxPending)
	e.replaySignal = make(chan struct{}, 1)
	e.ctx, e.cancel = context.WithCancel(context.Background())
	sid, err := mtproto.NewSessionID()
	if err != nil {
		return nil, err
	}
	e.sessionID.Store(sid)
	e.salt.Store(0) // server corrects via new_session_created / bad_server_salt

	// Connection-loss semantics: entering Reconnecting means every byte
	// handed to the old socket is in the ambiguity window. Default policy
	// (RetryOnReconnect=false) fails all pending requests deterministically
	// instead of blindly replaying them. The replay policy must NOT run
	// inline: subscription callbacks run on the transport's lifecycle
	// goroutine and must never block — the replay snapshot is recorded here
	// and executed by the dispatcher (recvLoop).
	tr.Subscribe(func(from, to transport.State) {
		if to == transport.StateReconnecting {
			if e.cfg.RetryOnReconnect {
				e.stageReplay()
			} else {
				e.reg.failAll(ErrConnectionLost)
			}
		}
		if to == transport.StateConnected && from == transport.StateReconnecting {
			// Fresh session: reference resets session ID and sequence.
			if sid, err := e.ids.resetSession(); err == nil {
				e.sessionID.Store(sid)
			}
			e.salt.Store(0)
		}
	})
	return e, nil
}

// Metrics exposes the counter block.
func (e *Engine) Metrics() *Metrics { return &e.metrics }

// Updates returns the bounded updates channel (Phase 7 will consume it).
// Overflow drops the incoming update and counts it — the dispatcher never
// blocks on update consumers.
func (e *Engine) Updates() <-chan tl.Object { return e.updates }

// Start launches the response dispatcher. Call after the transport is
// connected (Invoke before the transport is ready blocks in Send under the
// transport's backpressure, which is safe).
func (e *Engine) Start() {
	go e.recvLoop()
}

// Invoke executes one RPC request and returns its decoded result.
//
// Cancellation propagates end to end: ctx bounds both the transport send
// (queue backpressure) and the wait for the response; a cancelled request
// is removed from the registry and can never be completed afterwards —
// while a response that already won the race is returned instead.
func (e *Engine) Invoke(ctx context.Context, req tl.Request) (tl.Object, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline, ok := e.deadline(ctx); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	// Serialize up front: one buffer per request (reused encode buffer is
	// not possible across concurrent Invokes sharing a transport writer).
	w := tl.NewWriter(256)
	if err := req.Encode(w); err != nil {
		return nil, fmt.Errorf("rpc: encode: %w", err)
	}
	body := make([]byte, w.Len())
	copy(body, w.Bytes())

	call := &pending{done: make(chan struct{})}
	call.msgID, call.seqNo = e.ids.assign(true) // RPC requests are content-related
	call.body = body
	call.contentRelated = true
	if !e.reg.add(call) {
		call.finalize(nil, ErrPendingLimit)
		return nil, fmt.Errorf("rpc: invoke: %w", ErrPendingLimit)
	}
	call.state.Store(int32(stateRegistered))

	// Register before send: a fast response can never race registration.
	if err := e.sendCall(ctx, call); err != nil {
		// Send failed (backpressure rejection/cancellation/closed): the
		// request never reached the wire; fail deterministically. This is
		// not a "completed request" and is counted as cancelled.
		call.finalize(nil, err)
		e.reg.remove(call.msgID)
		e.metrics.RequestsCancelled.Add(1)
		return nil, err
	}

	select {
	case <-call.done:
		return call.result, call.err
	case <-ctx.Done():
		// Exactly-one-winner: try to cancel; if the response already
		// finalized the call, return that outcome instead.
		if call.finalize(nil, ctx.Err()) {
			e.reg.remove(call.msgID)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				e.metrics.RequestsTimedOut.Add(1)
			} else {
				e.metrics.RequestsCancelled.Add(1)
			}
			return nil, ctx.Err()
		}
		<-call.done
		return call.result, call.err
	}
}

// deadline returns the effective per-request timeout: the caller's context
// deadline when present, otherwise the configured default.
func (e *Engine) deadline(ctx context.Context) (time.Duration, bool) {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl), true
	}
	if e.cfg.DefaultTimeout > 0 {
		return e.cfg.DefaultTimeout, true
	}
	return 0, false
}

// sendCall frames one request into an encrypted envelope and enqueues it on
// the transport (honoring transport backpressure and ctx).
func (e *Engine) sendCall(ctx context.Context, call *pending) error {
	payload := e.frameMessage(call.msgID, call.seqNo, call.body, true)
	envelope, err := mtproto.EncryptMessageData(e.cfg.AuthKey, payload, e.cfg.Rand)
	if err != nil {
		return fmt.Errorf("rpc: seal envelope: %w", err)
	}
	if err := e.tr.Send(ctx, envelope); err != nil {
		return fmt.Errorf("rpc: transport send: %w", err)
	}
	call.state.Store(int32(stateSent))
	return nil
}

// frameMessage builds the plaintext payload: salt|session_id|TLMessage.
// The 16-byte message header (msg_id, seq_no, length) is written here and
// the length patched, mirroring tl.TLMessage.encode which is unexported for
// inbound use.
func (e *Engine) frameMessage(msgID int64, seqNo int32, body []byte, contentRelated bool) []byte {
	w := tl.NewWriter(16 + len(body) + 16)
	w.PutInt64(e.salt.Load())
	w.PutInt64(e.sessionID.Load())
	w.PutInt64(msgID)
	w.PutInt32(seqNo)
	w.PutInt32(int32(len(body)))
	w.PutRaw(body)
	return w.Bytes()
}

// resend re-sends an existing request under a fresh message ID (bad-salt
// recovery or RetryOnReconnect). The old registration is dropped first.
func (e *Engine) resend(call *pending) error {
	e.reg.remove(call.msgID)
	call.msgID, call.seqNo = e.ids.assign(call.contentRelated)
	if !e.reg.add(call) {
		call.finalize(nil, ErrPendingLimit)
		return ErrPendingLimit
	}
	call.state.Store(int32(stateRegistered))
	e.metrics.Resends.Add(1)
	return e.sendCall(e.ctx, call)
}

// stageReplay snapshots pending requests for replay (RetryOnReconnect).
// Non-blocking: called on the transport's lifecycle goroutine.
func (e *Engine) stageReplay() {
	snap := e.reg.snapshot()
	e.replayMu.Lock()
	e.replay = append(e.replay, snap...)
	e.replayMu.Unlock()
	select {
	case e.replaySignal <- struct{}{}:
	default:
	}
}

// drainReplay re-registers staged requests under fresh message IDs and
// re-enqueues them (reference behavior on reconnect). Called only from the
// dispatcher so Send backpressure can never wedge the lifecycle owner.
func (e *Engine) drainReplay() {
	e.replayMu.Lock()
	staged := e.replay
	e.replay = nil
	e.replayMu.Unlock()
	for _, p := range staged {
		if p.isDone() {
			continue // completed in the ambiguity window: ignore
		}
		if err := e.resend(p); err != nil {
			e.reg.finalizeByMsgID(p.msgID, nil, err)
		}
	}
}

// Close shuts the engine down: reject new requests, fail all pending
// deterministically, stop the dispatcher, release resources. The transport
// itself is owned by the caller and is not closed here.
func (e *Engine) Close() error {
	e.closeMu.Lock()
	if e.closed {
		e.closeMu.Unlock()
		return ErrEngineClosed
	}
	e.closed = true
	e.closeMu.Unlock()

	e.once.Do(func() { close(e.done) })
	e.cancel()
	n := e.reg.failAll(fmt.Errorf("%w: shutdown", ErrEngineClosed))
	_ = n
	return nil
}

// Pending reports the number of in-flight requests (diagnostics/tests).
func (e *Engine) Pending() int { return e.reg.len() }

// MaxPending reports the configured pending-request capacity.
func (e *Engine) MaxPending() int { return e.reg.pendingLimit() }

// DebugProtoErr returns the last stored decode error (temporary diagnostics).
func (e *Engine) DebugProtoErr() string {
	v, _ := debugProtoErr.Load().(string)
	return v
}
