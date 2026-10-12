package client_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// newTestClient returns a client with a pre-seeded auth key whose key
// material is also returned for the fake server wiring.
func newTestClient(t testing.TB, host string) (*client.Client, *crypto.AuthKey) {
	t.Helper()
	key := testAuthKeyOf(9)
	sess := session.NewWithKey(key)
	c, err := client.New(client.Config{
		Session:    sess,
		Host:       host,
		TLSEnabled: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func TestClientLifecycleStates(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, key := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, key)
	defer c.Close()

	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.State() != clientStateReady {
		t.Fatalf("state %s, want ready", c.State())
	}
	if !c.IsConnected() {
		t.Fatal("IsConnected false in Ready")
	}
	// Disconnect is graceful and reconnectable.
	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.State() != clientStateDisconnected {
		t.Fatalf("state after disconnect %s", c.State())
	}
	if c.IsConnected() {
		t.Fatal("IsConnected true after disconnect")
	}
	// Reconnect works and stays Ready.
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.State() != clientStateReady {
		t.Fatalf("state after reconnect %s", c.State())
	}
}

func TestConcurrentConnect(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, key := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, key)
	defer c.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Connect(context.Background())
		}()
	}
	wg.Wait()
	close(errs)
	ready := 0
	for err := range errs {
		if err == nil {
			ready++
			continue
		}
		if !errors.Is(err, client.ErrConnectInProgress) && !errors.Is(err, client.ErrClosed) {
			t.Fatalf("unexpected concurrent connect error: %v", err)
		}
	}
	if ready == 0 {
		t.Fatal("no Connect succeeded")
	}
	if s := c.State(); s != clientStateReady && s != clientStateConnected {
		t.Fatalf("post-race state %s", s)
	}
}

func TestConcurrentCloseAndDoubleClose(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, key := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, key)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = c.Close() }()
		go func() { defer wg.Done(); _ = c.Disconnect(context.Background()) }()
	}
	wg.Wait()
	_ = c.Close()
	if c.State() != clientStateClosed {
		t.Fatalf("state %s after concurrent close", c.State())
	}
	// Invoke after close fails cleanly.
	if _, err := c.Invoke(context.Background(), &pingLike{}); !errors.Is(err, client.ErrClosed) {
		t.Fatalf("invoke after close: %v", err)
	}
}

func TestConnectCancellation(t *testing.T) {
	c, _ := newTestClient(t, "10.255.255.1:65500")
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.Connect(ctx); err == nil {
		t.Fatal("expected cancellation error")
	}
	if c.State() != clientStateDisconnected {
		t.Fatalf("post-cancel state %s", c.State())
	}
	if c.LastError() == nil {
		t.Fatal("LastError not recorded")
	}
}

func TestConnectAfterDisconnectAndClose(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	c, key := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, key)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect after disconnect: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); !errors.Is(err, client.ErrClosed) {
		t.Fatalf("connect after close: %v", err)
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	baseline := runtime_NumGoroutine()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	for i := 0; i < 5; i++ {
		c, key := newTestClient(t, srv.Addr)
		wireFakeAppServer(t, srv, key)
		_ = c.Connect(context.Background())
		_, _ = c.Invoke(context.Background(), &pingLike{})
		_ = c.Close()
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime_NumGoroutine() <= baseline+4 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: %d > %d+4", runtime_NumGoroutine(), baseline)
}

// ---- RPC error model ------------------------------------------------------

func TestRPCErrorSurfaces(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return nil, &rpc.RPCError{Code: 400, Message: "PEER_ID_INVALID"}
	})

	_, err = c.Invoke(context.Background(), &pingLike{})
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != 400 || rpcErr.Message != "PEER_ID_INVALID" {
		t.Fatalf("error %v, want RPCError 400 PEER_ID_INVALID", err)
	}
}

func TestFloodWaitSurfaced(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return nil, &rpc.RPCError{Code: 420, Message: "FLOOD_WAIT_9999"}
	})

	_, err = c.Invoke(context.Background(), &pingLike{})
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Argument != 9999 {
		t.Fatalf("error %v, want FLOOD_WAIT_9999 surfaced", err)
	}
}

func TestFloodWaitRetriedBelowThreshold(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.SetFloodWaitThreshold(2 * time.Second)

	calls := 0
	var mu sync.Mutex
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil, &rpc.RPCError{Code: 420, Message: "FLOOD_WAIT_1"}
		}
		return &gen.Pong{PingID: 1}, nil
	})

	if _, err := c.Invoke(context.Background(), &pingLike{}); err != nil {
		t.Fatalf("flood retry failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls %d, want 2 (retry after flood wait)", calls)
	}
}

func TestServerInternalErrorRetried(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	calls := 0
	var mu sync.Mutex
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n < 3 {
			return nil, &rpc.RPCError{Code: 500, Message: "INTERNAL"}
		}
		return &gen.Pong{PingID: 1}, nil
	})

	if _, err := c.Invoke(context.Background(), &pingLike{}); err != nil {
		t.Fatalf("server-error retry failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("calls %d, want 3", calls)
	}
}

func TestContextCancellationWins(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.SetDelay(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.Invoke(ctx, &pingLike{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatal("cancellation did not propagate promptly")
	}
}
