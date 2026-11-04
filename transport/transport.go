// Package transport wires the Soroush Plus pipeline:
//
//	outbound: payload → abridged frame → obfuscated2 → WebSocket → wire
//	inbound:  wire → WebSocket message → de-obfuscate → abridged decode → payload
//
// The payloads are opaque MTProto envelopes: this layer does not touch
// msg_key, AES-IGE, or TL — those belong to the Phase 2 packages it sits
// beneath (github.com/Skryldev/suromesh/mtproto, github.com/Skryldev/suromesh/tl).
//
// Concurrency model (docs/concurrency.md is authoritative):
//   - exactly one lifecycle goroutine (run): owns dialing, reconnection,
//     and connection replacement;
//   - per connection: one reader goroutine (owns the inbound stream and the
//     abridged decoder), one writer goroutine (the only data writer), one
//     heartbeat goroutine (enqueues pings, checks staleness);
//   - bounded queues between producer/consumer with explicit policies.
package transport

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/transport/abridged"
	"github.com/Skryldev/suromesh/transport/ws"
)

// Policy selects outbound-queue-full behavior.
type Policy int

const (
	// PolicyBlock blocks the producer until space frees or a context ends.
	// This is end-to-end backpressure: a slow network slows producers.
	PolicyBlock Policy = iota
	// PolicyReject fails the Send immediately with ErrQueueFull.
	PolicyReject
)

func (p Policy) String() string {
	if p == PolicyReject {
		return "reject"
	}
	return "block"
}

// Config carries every tunable with production-safe defaults mirroring the
// reference implementation's cadence (30 s ping / 90 s recv timeout /
// 1800 s forced recycle) and queue bounds absent from the reference.
type Config struct {
	// Host is the "host:port" WebSocket endpoint.
	Host string

	// WS configures the handshake (path/origin/subprotocol defaults
	// reproduce the reference exactly).
	WS ws.Options

	// OutboundQueueSize bounds queued outbound packets (default 1024).
	OutboundQueueSize int
	// InboundQueueSize bounds queued inbound packets (default 1024).
	InboundQueueSize int
	// OutboundPolicy selects block-vs-reject on a full outbound queue
	// (default PolicyBlock).
	OutboundPolicy Policy

	// MaxPacketSize bounds a single abridged packet in each direction
	// (default 16 MiB, matching the reference WS message bound).
	MaxPacketSize int

	// PingInterval is the WebSocket keepalive cadence; 0 selects the
	// default (30 s, mirroring the reference) and a negative value
	// disables pings entirely (tests with deterministic byte streams).
	PingInterval time.Duration
	// RecvTimeout is the maximum silence before a connection is declared
	// dead (default 90 s).
	RecvTimeout time.Duration
	// MaxConnectionAge force-recycles a connection (default 1800 s like
	// the reference; 0 disables).
	MaxConnectionAge time.Duration
	// WriteTimeout bounds a single WebSocket write (default 30 s).
	WriteTimeout time.Duration
	// ConnectTimeout bounds dial+handshake (default 10 s).
	ConnectTimeout time.Duration

	// MaxReconnects bounds automatic reconnects per transport lifetime
	// (default 5, like the reference's connection_retries; negative means
	// unlimited — not recommended).
	MaxReconnects int
	// BackoffBase / BackoffMax / BackoffJitter shape the exponential
	// backoff (defaults 1 s / 30 s / 0.3).
	BackoffBase   time.Duration
	BackoffMax    time.Duration
	BackoffJitter float64

	// Rand supplies obfuscation-header entropy (default crypto/rand;
	// deterministic readers are for tests only).
	Rand io.Reader

	Hooks Hooks
}

func (c *Config) fill() {
	if c.OutboundQueueSize <= 0 {
		c.OutboundQueueSize = 1024
	}
	if c.InboundQueueSize <= 0 {
		c.InboundQueueSize = 1024
	}
	if c.MaxPacketSize <= 0 {
		c.MaxPacketSize = abridged.DefaultMaxPacketSize
	}
	if c.PingInterval == 0 {
		c.PingInterval = 30 * time.Second
	}
	if c.RecvTimeout <= 0 {
		c.RecvTimeout = 90 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.MaxReconnects == 0 {
		c.MaxReconnects = 5
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.BackoffJitter <= 0 {
		c.BackoffJitter = 0.3
	}
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
}

// outboundItem is one queued unit. control items carry no payload framing.
type outboundItem struct {
	payload  []byte
	ping     bool
	enqueued time.Time // for write-latency sampling
}

// Transport is a reconnecting Soroush WebSocket transport.
type Transport struct {
	cfg     Config
	sm      stateMachine
	metrics Metrics

	ctx    context.Context
	cancel context.CancelFunc

	outbound chan *outboundItem
	inbound  chan []byte

	firstConnected chan struct{}
	firstOnce      sync.Once
	done           chan struct{} // closed when run() exits
	doneOnce       sync.Once
	started        atomic.Bool  // true once Connect spawned run()
	terminalErr    atomic.Value // error

	runWG sync.WaitGroup // reader/writer/heartbeat of the live connection

	currentMu   sync.Mutex
	currentConn *ws.Conn // live connection handle (test seam)
}

func (t *Transport) closeDone() { t.doneOnce.Do(func() { close(t.done) }) }

// New creates a transport in StateNew. Call Connect to start it.
func New(cfg Config) *Transport {
	cfg.fill()
	t := &Transport{
		cfg:            cfg,
		outbound:       make(chan *outboundItem, cfg.OutboundQueueSize),
		inbound:        make(chan []byte, cfg.InboundQueueSize),
		firstConnected: make(chan struct{}),
		done:           make(chan struct{}),
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.sm.onDiff = func(from, to State) {
		if t.cfg.Hooks.OnStateChange != nil {
			t.cfg.Hooks.OnStateChange(from, to)
		}
	}
	return t
}

// Metrics exposes the counter block (always non-nil; snapshot for reads).
func (t *Transport) Metrics() *Metrics { return &t.metrics }

// Subscribe registers a callback invoked on every state transition.
// Additive integration point for the request engine; callbacks must not
// block (they run under the state lock). Registering after Connect is
// legal; the callback observes subsequent transitions only.
func (t *Transport) Subscribe(fn func(from, to State)) {
	t.sm.mu.Lock()
	t.sm.subs = append(t.sm.subs, fn)
	t.sm.mu.Unlock()
}

// State returns the current lifecycle state.
func (t *Transport) State() State { return t.sm.get() }

// Connect starts the transport and blocks until the first connection is
// established, the context ends, or the transport terminally fails.
func (t *Transport) Connect(ctx context.Context) error {
	if _, ok := t.sm.transition(StateNew, StateConnecting); !ok {
		return fmt.Errorf("%w: Connect from %s", ErrBadState, t.State())
	}
	t.started.Store(true)
	go t.run()

	select {
	case <-t.firstConnected:
		return nil
	case <-t.done:
		err, _ := t.terminalErr.Load().(error)
		if err != nil {
			return err
		}
		return ErrClosed
	case <-ctx.Done():
		// Caller gave up; shut the whole transport down.
		t.Close()
		return ctx.Err()
	case <-t.ctx.Done():
		err, _ := t.terminalErr.Load().(error)
		if err != nil {
			return err
		}
		return ErrClosed
	}
}

// Send queues one payload (an opaque MTProto envelope) for transmission.
// It confirms enqueueing, not transmission; delivery guarantees are the
// request engine's job (Phase 4).
//
// Under PolicyBlock (default) a full queue blocks until space, the context
// ends, or the transport closes — end-to-end backpressure. Under
// PolicyReject it fails fast with ErrQueueFull.
func (t *Transport) Send(ctx context.Context, payload []byte) error {
	switch t.State() {
	case StateClosed:
		return ErrClosed
	case StateNew:
		return ErrNotConnected
	}
	item := &outboundItem{payload: payload, enqueued: time.Now()}
	if t.cfg.OutboundPolicy == PolicyReject {
		select {
		case t.outbound <- item:
			return nil
		default:
			t.metrics.QueueFullEvents.Add(1)
			return &Error{Class: ClassResource, Op: "send", Err: ErrQueueFull}
		}
	}
	select {
	case t.outbound <- item:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ctx.Done():
		return ErrClosed
	}
}

// Inbound exposes the bounded inbound packet channel directly so integrators
// (the request engine) can select over packets and other events. The Recv
// helper wraps it for simple consumption. Do not close the channel; it is
// closed when the transport stops.
func (t *Transport) Inbound() <-chan []byte { return t.inbound }

// Recv returns the next inbound packet. It blocks until a packet arrives,
// the context ends, or the transport closes (buffered packets are drained
// first, then ErrClosed).
func (t *Transport) Recv(ctx context.Context) ([]byte, error) {
	for {
		select {
		case p := <-t.inbound:
			t.metrics.PacketsDelivered.Add(1)
			return p, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.done:
			select {
			case p := <-t.inbound:
				t.metrics.PacketsDelivered.Add(1)
				return p, nil
			default:
				return nil, ErrClosed
			}
		}
	}
}

// Close shuts the transport down deterministically:
//
//	state → Closed (rejects new work) → cancel lifecycle context →
//	run() closes the live connection and waits for its goroutines →
//	blocked Sends/Recvs unblock → run() exits → Close returns.
//
// It is safe to Close a transport that was never Connected. Idempotent;
// the second call returns ErrClosed.
func (t *Transport) Close() error {
	if _, ok := t.sm.transitionAny(StateClosed); !ok {
		return ErrClosed
	}
	t.cancel()
	if t.started.Load() {
		<-t.done
	} else {
		// run() never started; nothing owns done. The once guards the
		// race where Connect starts run() concurrently.
		t.closeDone()
	}
	return nil
}

// Done is closed when the transport has fully stopped.
func (t *Transport) Done() <-chan struct{} { return t.done }

// TerminalErr reports why the transport stopped (nil for user Close).
func (t *Transport) TerminalErr() error {
	err, _ := t.terminalErr.Load().(error)
	return err
}

// ---- lifecycle owner ------------------------------------------------------

func (t *Transport) run() {
	defer t.closeDone()
	// Cancel the lifecycle context on any exit path so blocked
	// Senders/Recvs waiting on it always unblock.
	defer t.cancel()
	attempts := 0

	for {
		select {
		case <-t.ctx.Done():
			return
		default:
		}

		conn, streams, err := t.dial()
		if err != nil {
			class := Classify(err)
			t.observeError(err, class)
			if class == ClassCancelled || errors.Is(err, context.Canceled) {
				t.setTerminal(err)
				t.sm.transitionAny(StateClosed)
				return
			}
			attempts++
			if !t.reconnectBudget(attempts) {
				t.setTerminal(fmt.Errorf("%w after %d attempt(s): %w",
					ErrReconnectExhausted, attempts, err))
				t.sm.transitionAny(StateClosed)
				return
			}
			t.metrics.ReconnectFailures.Add(1)
			if t.sm.get() == StateConnecting {
				t.sm.transition(StateConnecting, StateReconnecting)
			}
			if !t.sleepBackoff(attempts) {
				return
			}
			continue
		}

		// Connection established.
		t.metrics.ConnOpened.Add(1)
		t.firstOnce.Do(func() { close(t.firstConnected) })
		if st := t.sm.get(); st == StateConnecting {
			t.sm.transition(StateConnecting, StateConnected)
		} else {
			t.sm.transition(StateReconnecting, StateConnected)
			t.metrics.Reconnects.Add(1)
		}
		attempts = 0

		if stopped := t.serveConn(conn, streams); stopped {
			t.sm.transitionAny(StateClosed) // terminal: violation, cancel, or user close
			return                          // terminal already recorded (or user close)
		}
		// Connection lost: recycle unless the user closed concurrently.
		t.metrics.ConnClosed.Add(1)
		if t.sm.get() == StateClosed {
			return
		}
		t.sm.transition(StateConnected, StateReconnecting)
	}
}

// dial establishes one connection: WebSocket handshake, obfuscation
// derivation, and the 64-byte header as the first binary message.
func (t *Transport) dial() (*ws.Conn, *crypto.ObfuscatedStream, error) {
	dialCtx, cancel := context.WithTimeout(t.ctx, t.cfg.ConnectTimeout)
	defer cancel()
	conn, err := ws.Dial(dialCtx, t.cfg.Host, t.cfg.WS)
	if err != nil {
		return nil, nil, err
	}
	header, streams, err := crypto.InitObfuscated(t.cfg.Rand, crypto.AbridgedTag)
	if err != nil {
		conn.Close(ws.CloseAbnormal, "")
		return nil, nil, err
	}
	conn.SetWriteDeadline(time.Now().Add(t.cfg.WriteTimeout))
	if err := conn.WriteMessage(ws.OpBinary, header); err != nil {
		conn.Close(ws.CloseAbnormal, "")
		return nil, nil, err
	}
	return conn, streams, nil
}

// serveConn runs the per-connection goroutines until the connection fails,
// ages out, or the transport stops. Returns true when the whole transport
// must stop (terminal error recorded or user close).
func (t *Transport) serveConn(conn *ws.Conn, streams *crypto.ObfuscatedStream) (terminal bool) {
	connCtx, cancelConn := context.WithCancel(t.ctx)
	defer cancelConn()
	t.currentMu.Lock()
	t.currentConn = conn
	t.currentMu.Unlock()
	defer func() {
		t.currentMu.Lock()
		t.currentConn = nil
		t.currentMu.Unlock()
	}()

	errCh := make(chan error, 3) // reader + writer + heartbeat
	var once sync.Once
	report := func(err error) {
		once.Do(func() { errCh <- err })
	}

	t.runWG.Add(3)
	go t.reader(connCtx, conn, streams, report)
	go t.writer(connCtx, conn, streams, report)
	go t.heartbeat(connCtx, report)

	var ageTimer <-chan time.Time
	if t.cfg.MaxConnectionAge > 0 {
		timer := time.NewTimer(t.cfg.MaxConnectionAge)
		defer timer.Stop()
		ageTimer = timer.C
	}

	select {
	case err := <-errCh:
		class := Classify(err)
		t.observeError(err, class)
		cancelConn()
		conn.Close(ws.CloseGoingAway, "recycle") // unblocks reader
		t.runWG.Wait()
		if class == ClassCancelled || errors.Is(err, context.Canceled) {
			t.setTerminalIfEmpty(err)
			return true
		}
		if class == ClassProtocol {
			// A peer that violates the wire format will keep violating it;
			// reconnect loops here only amplify damage.
			t.setTerminalIfEmpty(fmt.Errorf("%w: %w", ErrProtocolFatal, err))
			return true
		}
		return false // temporary/timeout/closed → reconnect

	case <-ageTimer:
		// Forced recycle (reference: 1800 s cadence). Queued-but-unsent
		// work survives; bytes already on the wire do not (the request
		// engine owns retransmission).
		cancelConn()
		conn.Close(ws.CloseGoingAway, "max age")
		t.runWG.Wait()
		return false

	case <-t.ctx.Done():
		cancelConn()
		conn.Close(ws.CloseNormal, "shutdown")
		t.runWG.Wait()
		t.drainOutbound()
		return true
	}
}

// reader owns the inbound direction: WebSocket messages → de-obfuscate →
// abridged packets → bounded inbound queue (blocking push = backpressure).
//
// Packets returned by the decoder alias its internal buffer, so each
// enqueued packet is copied once at the queue boundary — the copy is what
// makes queue ownership safe; the cost is tracked in docs/performance.md.
func (t *Transport) reader(ctx context.Context, conn *ws.Conn, streams *crypto.ObfuscatedStream, report func(error)) {
	defer t.runWG.Done()
	dec := abridged.NewDecoder(t.cfg.MaxPacketSize)
	var packets [][]byte

	for {
		start := time.Now()
		// RecvTimeout as a rolling read deadline: any inbound frame
		// (data or pong) resets it on the next iteration.
		conn.SetReadDeadline(time.Now().Add(t.cfg.RecvTimeout))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			report(err)
			return
		}
		t.metrics.BytesReceived.Add(uint64(len(msg)))
		clearBytes := streams.Decrypt(msg)
		packets, err = dec.Feed(clearBytes, packets[:0])
		if err != nil {
			t.metrics.ProtocolErrors.Add(1)
			report(fmt.Errorf("abridged: %w", err))
			return
		}
		for _, p := range packets {
			owned := append(make([]byte, 0, len(p)), p...)
			t.metrics.PacketsReceived.Add(1)
			select {
			case t.inbound <- owned:
			case <-ctx.Done():
				return
			}
		}
		if t.cfg.Hooks.OnReadLatency != nil {
			t.cfg.Hooks.OnReadLatency(time.Since(start))
		}
		packets = packets[:0]
	}
}

// writer is the single data writer: outbound queue → abridged →
// obfuscate → WebSocket frame. Control (ping) items bypass framing.
func (t *Transport) writer(ctx context.Context, conn *ws.Conn, streams *crypto.ObfuscatedStream, report func(error)) {
	defer t.runWG.Done()
	var frame []byte // reused across writes

	for {
		select {
		case <-ctx.Done():
			return
		case item := <-t.outbound:
			if item.ping {
				conn.SetWriteDeadline(time.Now().Add(t.cfg.WriteTimeout))
				if err := conn.WriteMessage(ws.OpPing, item.payload); err != nil {
					report(err)
					return
				}
				t.metrics.PingsSent.Add(1)
				continue
			}
			var err error
			frame, err = abridged.Encode(frame[:0], item.payload)
			if err != nil {
				t.metrics.ProtocolErrors.Add(1)
				report(fmt.Errorf("abridged encode: %w", err))
				return
			}
			out := streams.Encrypt(frame)
			conn.SetWriteDeadline(time.Now().Add(t.cfg.WriteTimeout))
			if err := conn.WriteMessage(ws.OpBinary, out); err != nil {
				report(err)
				return
			}
			t.metrics.PacketsSent.Add(1)
			t.metrics.BytesSent.Add(uint64(len(out)))
			if t.cfg.Hooks.OnWriteLatency != nil {
				t.cfg.Hooks.OnWriteLatency(time.Since(item.enqueued))
			}
		}
	}
}

// heartbeat enqueues pings and samples health. Pings share the outbound
// queue; if it is saturated with real traffic, skipping a ping is correct
// (the traffic itself proves liveness).
func (t *Transport) heartbeat(ctx context.Context, report func(error)) {
	defer t.runWG.Done()
	if t.cfg.PingInterval <= 0 {
		return
	}
	ticker := time.NewTicker(t.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if t.cfg.Hooks.OnQueueDepth != nil {
				t.cfg.Hooks.OnQueueDepth(len(t.outbound), len(t.inbound))
			}
			var ping [8]byte
			if _, err := io.ReadFull(t.cfg.Rand, ping[:]); err != nil {
				report(fmt.Errorf("ping entropy: %w", err))
				return
			}
			select {
			case t.outbound <- &outboundItem{ping: true, payload: ping[:], enqueued: time.Now()}:
			default:
				// Queue saturated: liveness is proven by the traffic itself.
			}
		}
	}
}

// ---- helpers --------------------------------------------------------------

func (t *Transport) reconnectBudget(attempts int) bool {
	if t.cfg.MaxReconnects < 0 {
		return true
	}
	return attempts <= t.cfg.MaxReconnects
}

// sleepBackoff waits exp(attempts) with jitter, honoring cancellation.
func (t *Transport) sleepBackoff(attempts int) bool {
	d := t.cfg.BackoffBase << uint(min(attempts-1, 16))
	if d > t.cfg.BackoffMax || d <= 0 {
		d = t.cfg.BackoffMax
	}
	j := 1 + (mathrand.Float64()*2-1)*t.cfg.BackoffJitter
	wait := time.Duration(float64(d) * j)
	if wait < 0 {
		wait = d
	}
	tm := time.NewTimer(wait)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-t.ctx.Done():
		return false
	}
}

func (t *Transport) drainOutbound() {
	for {
		select {
		case <-t.outbound:
		default:
			return
		}
	}
}

func (t *Transport) setTerminal(err error) {
	t.terminalErr.CompareAndSwap(nil, err)
}

func (t *Transport) setTerminalIfEmpty(err error) {
	if t.terminalErr.Load() == nil && err != nil && !errors.Is(err, context.Canceled) {
		t.terminalErr.CompareAndSwap(nil, err)
	}
}

func (t *Transport) observeError(err error, class Classification) {
	if t.cfg.Hooks.OnError != nil {
		t.cfg.Hooks.OnError(err, class)
	}
	if class == ClassProtocol {
		t.metrics.ProtocolErrors.Add(1)
	}
}

// ForceReconnectForTest closes the active connection so the run loop
// reconnects (test seam for reconnect-invariant tests; equivalent to a
// connection loss).
func (t *Transport) ForceReconnectForTest() {
	t.currentMu.Lock()
	conn := t.currentConn
	t.currentMu.Unlock()
	if conn != nil {
		_ = conn.Close(1000, "test reconnect")
	}
}
