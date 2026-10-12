package transport_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/transport"
)

// requireGoroutinesBack polls until the goroutine count returns to
// baseline (within slack) or fails. Call only after the transport and
// server are closed.
func requireGoroutinesBack(t *testing.T, baseline int, slack int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if n := runtime.NumGoroutine(); n <= baseline+slack {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to baseline: %d > %d+%d",
		runtime.NumGoroutine(), baseline, slack)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}

func waitForPeer(t *testing.T, peers <-chan *wstest.SoroushPeer) *wstest.SoroushPeer {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case p := <-peers:
			return p
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("no peer connection within 3s")
	return nil
}

func newSoroushServer(t *testing.T, onPacket func([]byte)) (*wstest.Server, chan *wstest.SoroushPeer) {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	peers := make(chan *wstest.SoroushPeer, 8)
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, func(_ *wstest.SoroushPeer, pkt []byte) {
			onPacket(pkt)
		})
		peers <- peer
		peer.Wait()
	}
	return srv, peers
}

// packet builds an MTProto-envelope-shaped payload: a multiple of 4 bytes,
// as the abridged codec requires (real envelopes are 16-byte aligned).
func packet(tag byte) []byte {
	p := make([]byte, 64)
	for i := range p {
		p[i] = tag
	}
	return p
}

func TestServerDisconnectTriggersReconnect(t *testing.T) {
	baseline := runtime.NumGoroutine()
	var mu sync.Mutex
	gotPackets := 0
	srv, peers := newSoroushServer(t, func([]byte) {
		mu.Lock()
		gotPackets++
		mu.Unlock()
	})

	tr := transport.New(testConfig(srv.Addr))
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	// First connection works.
	if err := tr.Send(context.Background(), packet(1)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return gotPackets >= 1 })

	// The server drops the connection; the transport must re-establish.
	peer := waitForPeer(t, peers)
	peer.Close()

	peer2 := waitForPeer(t, peers)
	if peer2 == peer {
		t.Fatal("expected a fresh connection after reconnect")
	}
	if err := tr.Send(context.Background(), packet(2)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return gotPackets >= 2 })

	if tr.Metrics().Reconnects.Load() < 1 {
		t.Fatal("reconnect not counted")
	}
	tr.Close()
	srv.Close()
	requireGoroutinesBack(t, baseline, 2)
}

func TestProtocolViolationIsFatal(t *testing.T) {
	baseline := runtime.NumGoroutine()
	srv, peers := newSoroushServer(t, nil)
	defer srv.Close()

	cfg := testConfig(srv.Addr)
	cfg.MaxPacketSize = 1024
	tr := transport.New(cfg)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Wait for the fake peer (and its decrypt streams) to be live, then
	// send a properly encrypted packet whose body is 2048 bytes — twice
	// the client's MaxPacketSize. The abridged decoder must reject it as
	// a fatal protocol violation.
	peer := waitForPeer(t, peers)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = peer.SendPacket(make([]byte, 2048))
	}()

	select {
	case <-tr.Done():
		if !errors.Is(tr.TerminalErr(), transport.ErrProtocolFatal) {
			t.Fatalf("terminal error %v, want protocol fatal", tr.TerminalErr())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("transport did not stop on protocol violation")
	}
	tr.Close()
	srv.Close()
	requireGoroutinesBack(t, baseline, 2)
}

func TestReconnectExhaustion(t *testing.T) {
	baseline := runtime.NumGoroutine()
	cfg := transport.Config{
		Host:             "127.0.0.1:1", // nothing listens
		WS:               wstest.WSOptions(),
		PingInterval:     -1,
		MaxReconnects:    2,
		BackoffBase:      5 * time.Millisecond,
		BackoffMax:       20 * time.Millisecond,
		MaxConnectionAge: 0,
	}
	tr := transport.New(cfg)
	err := tr.Connect(context.Background())
	if err == nil {
		t.Fatal("expected initial connect failure")
	}
	if !errors.Is(err, transport.ErrReconnectExhausted) {
		t.Fatalf("error %v, want ErrReconnectExhausted", err)
	}
	if tr.Metrics().ReconnectFailures.Load() < 2 {
		t.Fatal("expected at least 2 recorded failures")
	}
	requireGoroutinesBack(t, baseline, 1)
}

func TestConnectCancellation(t *testing.T) {
	baseline := runtime.NumGoroutine()
	cfg := transport.Config{
		Host:           "10.255.255.1:65500", // blackhole
		WS:             wstest.WSOptions(),
		PingInterval:   -1,
		ConnectTimeout: 10 * time.Second,
	}
	tr := transport.New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := tr.Connect(ctx); err == nil {
		t.Fatal("expected cancellation error")
	}
	tr.Close()
	requireGoroutinesBack(t, baseline, 2)
}

// stallServer creates a transport whose peer died and whose dial always
// fails (the fake server's listener is closed, so reconnects to its address
// are refused): the manager is parked in backoff and nothing drains the
// outbound queue — the deterministic precondition for queue-policy tests.
func stallServer(t *testing.T, cfg transport.Config) *transport.Transport {
	t.Helper()
	srv, peers := newSoroushServer(t, nil)
	cfg.Host = srv.Addr // reconnects hit the closed listener and refuse
	tr := transport.New(cfg)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	peer := waitForPeer(t, peers)
	peer.Close()
	srv.Close() // subsequent dials are refused → manager parks in backoff
	// Deterministic precondition: wait until the transport has actually
	// entered Reconnecting (the old writer goroutine is gone — nothing
	// drains the outbound queue from here on).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tr.State() == transport.StateReconnecting {
			return tr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("transport never entered Reconnecting")
	return tr
}

func TestOutboundQueueRejectPolicy(t *testing.T) {
	baseline := runtime.NumGoroutine()
	cfg := testConfig("")
	cfg.OutboundQueueSize = 2
	cfg.OutboundPolicy = transport.PolicyReject
	cfg.BackoffBase = 5 * time.Second // park long enough to fill the queue
	cfg.BackoffMax = 5 * time.Second
	tr := stallServer(t, cfg)
	defer tr.Close()

	var rejects int
	for i := 0; i < 16; i++ {
		err := tr.Send(context.Background(), packet(byte(i)))
		if errors.Is(err, transport.ErrQueueFull) {
			rejects++
		} else if err != nil {
			t.Fatalf("unexpected send error: %v", err)
		}
	}
	if rejects == 0 {
		t.Fatal("no rejections observed with a full queue")
	}
	if tr.Metrics().QueueFullEvents.Load() == 0 {
		t.Fatal("queue saturation not counted")
	}
	requireGoroutinesBack(t, baseline, 2)
}

func TestOutboundQueueBlockPolicyAndShutdown(t *testing.T) {
	baseline := runtime.NumGoroutine()
	cfg := testConfig("")
	cfg.OutboundQueueSize = 1
	cfg.OutboundPolicy = transport.PolicyBlock
	cfg.BackoffBase = 5 * time.Second
	cfg.BackoffMax = 5 * time.Second
	tr := stallServer(t, cfg)

	blocked := make(chan error, 1)
	go func() {
		// Fill the queue (1 slot), then the next send must block; Close()
		// must unblock it.
		for i := 0; i < 8; i++ {
			if err := tr.Send(context.Background(), packet(byte(i))); err != nil {
				blocked <- err
				return
			}
		}
		blocked <- nil
	}()

	time.Sleep(100 * time.Millisecond) // let the queue fill
	tr.Close()                         // shutdown while a send is blocked
	select {
	case err := <-blocked:
		if err == nil {
			t.Fatal("sends drained despite a dead connection; backpressure did not engage")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("blocked send did not unblock on shutdown")
	}
	requireGoroutinesBack(t, baseline, 2)
}

func TestContextCancellationDuringSend(t *testing.T) {
	baseline := runtime.NumGoroutine()
	cfg := testConfig("")
	cfg.OutboundQueueSize = 1
	cfg.BackoffBase = 5 * time.Second
	cfg.BackoffMax = 5 * time.Second
	tr := stallServer(t, cfg)
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_ = tr.Send(context.Background(), packet(1)) // fills the single slot
	err := tr.Send(ctx, packet(2))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("send error %v, want context.Canceled", err)
	}
	requireGoroutinesBack(t, baseline, 2)
}

func TestSlowServerBackpressureBounded(t *testing.T) {
	baseline := runtime.NumGoroutine()
	srv, peers := newSoroushServer(t, nil)
	defer srv.Close()

	// Nobody consumes tr.inbound via Recv; the peer floods packets until
	// the client's bounded inbound queue fills and the reader blocks —
	// the peer's writes then stall on TCP backpressure. Memory stays
	// bounded by InboundQueueSize + one in-flight frame.
	cfg := testConfig(srv.Addr)
	cfg.InboundQueueSize = 8
	tr := transport.New(cfg)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	peer := waitForPeer(t, peers)

	// The peer's decrypt streams exist only after it processed the client's
	// 64-byte header; wait for that before flooding.
	waitFor(t, peer.HasHeader)

	var sent int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := peer.SendPacket(packet(3)); err != nil {
			break // stalled: backpressure engaged
		}
		sent++
	}
	if sent < 8 {
		t.Fatalf("flood stopped after %d packets — expected at least the queue bound", sent)
	}
	// The transport must still close cleanly (no deadlock, no leak).
	tr.Close()
	srv.Close()
	requireGoroutinesBack(t, baseline, 2)
}

func TestPartialWriteFromServer(t *testing.T) {
	baseline := runtime.NumGoroutine()
	srv, peers := newSoroushServer(t, nil)

	tr := transport.New(testConfig(srv.Addr))
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	peer := waitForPeer(t, peers)
	// Truncated frame: the client reader blocks mid-frame. Closing the
	// peer surfaces EOF and triggers a reconnect (never a hang).
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = peer.Raw().TruncateHalf(0x2, make([]byte, 1024))
	}()
	time.Sleep(100 * time.Millisecond)
	peer.Close()

	waitForPeer(t, peers) // second connection proves recovery

	tr.Close()
	srv.Close()
	requireGoroutinesBack(t, baseline, 2)
}

func TestRepeatedConnectDisconnectCycles(t *testing.T) {
	baseline := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		srv, _ := wstest.New()
		tr := transport.New(testConfig(srv.Addr))
		if err := tr.Connect(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		tr.Close()
		srv.Close()
		select {
		case <-tr.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("cycle %d: transport did not stop", i)
		}
	}
	requireGoroutinesBack(t, baseline, 2)
}

func TestShutdownDuringReconnectBackoff(t *testing.T) {
	baseline := runtime.NumGoroutine()
	srv, peers := newSoroushServer(t, nil)
	defer srv.Close()

	cfg := testConfig(srv.Addr)
	cfg.BackoffBase = time.Second
	cfg.BackoffMax = time.Second
	tr := transport.New(cfg)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	peer := waitForPeer(t, peers)
	peer.Close() // forces a reconnect with a 1s backoff

	time.Sleep(50 * time.Millisecond) // enter backoff
	start := time.Now()
	tr.Close()
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Close took %v during backoff; should cancel promptly", elapsed)
	}
	requireGoroutinesBack(t, baseline, 2)
}

func TestHeartbeatPingDelivered(t *testing.T) {
	baseline := runtime.NumGoroutine()
	srv, _ := wstest.New()
	defer srv.Close()

	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, nil)
		peer.Wait()
	}

	// The interval is configurable; this asserts repeated pings flow
	// through the writer without disturbing data traffic.
	cfg := testConfig(srv.Addr)
	cfg.PingInterval = 30 * time.Millisecond
	tr := transport.New(cfg)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if tr.Metrics().PingsSent.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if tr.Metrics().PingsSent.Load() < 2 {
		t.Fatal("heartbeat did not send repeated pings")
	}
	tr.Close()
	requireGoroutinesBack(t, baseline, 2)
}

func TestShutdownDuringRead(t *testing.T) {
	baseline := runtime.NumGoroutine()
	srv, peers := newSoroushServer(t, nil)

	cfg := testConfig(srv.Addr)
	cfg.RecvTimeout = time.Minute // reader would idle for a minute
	tr := transport.New(cfg)
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = waitForPeer(t, peers)

	// The reader is parked in ReadMessage; Close must terminate it via the
	// connection close, not wait for the read timeout.
	start := time.Now()
	tr.Close()
	srv.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v with a parked reader", elapsed)
	}
	requireGoroutinesBack(t, baseline, 2)
}
