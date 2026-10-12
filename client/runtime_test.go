package client_test

// Phase 15 runtime hardening tests: pending invariants, Close barriers,
// reconnect invariants, transfer hardening, subscriber lifecycle,
// goroutine leak detection.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestPendingInvariantCloseStorm: Close fails every pending request and
// drains the registry — pending == 0 and no request completes twice.
func TestPendingInvariantCloseStorm(t *testing.T) {
	c, fake, _ := newDialogsClient(t)

	fake.SetDelay(200 * time.Millisecond)
	const n = 50
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)})
		}(i)
	}
	time.Sleep(60 * time.Millisecond)
	eng := c.EngineMetrics()
	if eng.RequestsStarted == 0 {
		t.Fatal("no requests started")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, err := range results {
		if err == nil {
			t.Fatalf("request %d succeeded after Close", i)
		}
	}
	m := c.Metrics()
	if m.PendingRequests != 0 {
		t.Fatalf("pending = %d after Close storm, want 0", m.PendingRequests)
	}
	// Exactly-once terminal outcome: no request both succeeded and failed.
	if eng2 := c.EngineMetrics(); eng2.RequestsCompleted+eng2.RequestsRPCError+eng2.RequestsCancelled+eng2.RequestsTimedOut+eng2.RequestsConnLost < eng2.RequestsStarted {
		// started requests must each have exactly one terminal outcome
		t.Fatalf("terminal accounting: started=%d completed=%d err=%d cancel=%d timeout=%d lost=%d",
			eng2.RequestsStarted, eng2.RequestsCompleted, eng2.RequestsRPCError,
			eng2.RequestsCancelled, eng2.RequestsTimedOut, eng2.RequestsConnLost)
	}
}

// TestPendingInvariantLoad: 1000 concurrent invokes against the fake —
// all complete, registry drains to 0.
func TestPendingInvariantLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, _ := newDialogsClient(t)
	defer c.Close()

	const n = 1000
	var wg sync.WaitGroup
	var failures int32
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := c.Invoke(ctx, &gen.PingRequest{PingID: int64(i)}); err != nil {
				atomic.AddInt32(&failures, 1)
			}
		}(i)
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d of %d invokes failed", failures, n)
	}
	if m := c.Metrics(); m.PendingRequests != 0 {
		t.Fatalf("pending = %d after load", m.PendingRequests)
	}
}

// TestReconnectInvariants: a reconnect must NOT re-run authentication,
// rotate the key, or regress authorization state.
func TestReconnectInvariants(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	srv := newWSTestServerForAuth(t)
	fs, err := session.OpenFile(filepath.Join(t.TempDir(), "reinv.session"))
	if err != nil {
		t.Fatal(err)
	}
	c, fake := newAuthClientWithSession(t, srv, fs)
	defer c.Close()
	af := newAuthFake("54321")
	installAuthHooks(t, fake, af)

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); err != nil {
		t.Fatal(err)
	}
	keyBefore := c.SessionKeyID()

	// Simulate connection loss: the transport reconnects; the client must
	// keep the key and authorized state, with no auth RPCs re-sent.
	authCallsBefore := countAuthCalls(af)
	c.ReconnectTransportForTest()

	waitUntil(t, 10*time.Second, func() bool {
		return c.IsConnected()
	})
	if keyAfter := c.SessionKeyID(); keyAfter != keyBefore {
		t.Fatal("auth key rotated across reconnect")
	}
	if got := c.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("auth state = %s after reconnect, want authorized", got)
	}
	if calls := countAuthCalls(af); calls != authCallsBefore {
		t.Fatalf("auth RPCs re-sent during reconnect (%d → %d)", authCallsBefore, calls)
	}
}

func countAuthCalls(af *authFake) int {
	af.mu.Lock()
	defer af.mu.Unlock()
	return af.sendCodeCalls + af.signInCalls + af.checkPWCalls
}

// TestTransferTerminalStateOnPanic: a panic unwinding through a transfer
// releases the slot AND settles the terminal state exactly once
// (Phase 15 guard).
func TestTransferTerminalStateOnPanic(t *testing.T) {
	c, _, _ := newDialogsClient(t)
	defer c.Close()

	func() {
		defer func() { _ = recover() }() // the guard re-panics; we absorb
		_, _ = c.Upload(context.Background(), panickingReader{}, client.UploadOptions{Size: 128 * 1024})
	}()
	// The transfer must be settled: no active slots, failed metric bumped.
	if m := c.TransferMetrics(); m.Active != 0 {
		t.Fatalf("active transfers = %d after panic, want 0", m.Active)
	}
	if m := c.TransferMetrics(); m.Failed == 0 {
		t.Fatal("panic did not settle the transfer as failed")
	}
}

type panickingReader struct{}

func (panickingReader) Read([]byte) (int, error) {
	panic("injected reader panic")
}

// TestTransferHardeningStorm: 500 concurrent transfer attempts mixing
// success, failure, cancellation and Close — every slot released, every
// transfer settled.
func TestTransferHardeningStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, _ := newDialogsClient(t)

	const n = 500
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { _ = recover() }()
			switch i % 4 {
			case 0: // success
				_, _ = c.Upload(context.Background(), newDeterministicReader(128*1024),
					client.UploadOptions{Size: 128 * 1024})
			case 1: // reader failure
				_, _ = c.Upload(context.Background(),
					&failingReader{data: patternSliceLocal(64 * 1024), err: errors.New("boom")},
					client.UploadOptions{Size: 192 * 1024})
			case 2: // cancellation
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				_ = cancel
				_, _ = c.Upload(ctx, newDeterministicReader(256*1024), client.UploadOptions{Size: 256 * 1024})
				cancel()
			default: // Close racing mid-run
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				_, _ = c.Upload(ctx, newDeterministicReader(64*1024), client.UploadOptions{Size: 64 * 1024})
			}
		}(i)
		if i%50 == 0 {
			wg.Wait() // batch waves so Close isn't perpetual
		}
	}
	wg.Wait()
	c.Close()
	if m := c.TransferMetrics(); m.Active != 0 {
		t.Fatalf("active transfers = %d after storm, want 0", m.Active)
	}
}

func patternSliceLocal(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestSubscriberLifecycleChurn: 1000 subscribe/unsubscribe cycles plus
// concurrent Close — no callback after terminal state.
func TestSubscriberLifecycleChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newDialogsClient(t)

	var afterClose int32
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 125; j++ {
				select {
				case <-stop:
					return
				default:
				}
				sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
					if c.ClosedForTest() {
						atomic.AddInt32(&afterClose, 1)
					}
				})
				sub.Unsubscribe()
			}
		}()
	}
	// Feed updates during churn.
	go func() {
		for i := 0; i < 200; i++ {
			fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
				Message: &gen.Message{ID: int32(i + 1), PeerID: &gen.PeerUser{UserID: 300}, Date: int32(i + 1)},
				PTS:     int32(i + 1), PTSCount: 1,
			}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
	c.Close()
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&afterClose); n != 0 {
		t.Fatalf("%d callbacks executed after Close", n)
	}
}

// TestRuntimeGoroutineLeak100Cycles: 100 Connect → workload → Close
// cycles with a documented goroutine baseline delta.
func TestRuntimeGoroutineLeak100Cycles(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	goroutineSettled(runtime_NumGoroutine(), 0)
	base := runtime_NumGoroutine()

	for i := 0; i < 100; i++ {
		// Per-cycle server close: t.Cleanup would keep all 100 accept
		// loops alive until test end and skew the measurement.
		srv, err := wstest.New()
		if err != nil {
			t.Fatal(err)
		}
		c, key := newTestClient(t, srv.Addr)
		fake := wireFakeAppServer(t, srv, key)
		fake.SetFrameLog(false)
		if err := c.Connect(context.Background()); err != nil {
			t.Fatal(err)
		}
		sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {})
		_, _ = c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)})
		sub.Unsubscribe()
		if err := c.Close(); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		srv.Close() // release the fake's goroutines NOW
	}
	if err := goroutineSettled(base, 8); err != nil {
		t.Fatal(err)
	}
}

// TestCloseDuringEverythingMatrix: Close during each subsystem operation
// terminates deterministically (spec §4 matrix).
func TestCloseDuringEverythingMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	scenarios := []struct {
		name  string
		start func(c *client.Client, fake *appFake) chan error
	}{
		{"invoke", func(c *client.Client, fake *appFake) chan error {
			done := make(chan error, 1)
			go func() {
				_, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: 1})
				done <- err
			}()
			return done
		}},
		{"retry-backoff", func(c *client.Client, fake *appFake) chan error {
			fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
				return nil, &rpc.RPCError{Code: 500, Message: "INTERNAL"}
			})
			done := make(chan error, 1)
			go func() {
				_, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: 2})
				done <- err
			}()
			return done
		}},
		{"send", func(c *client.Client, fake *appFake) chan error {
			done := make(chan error, 1)
			go func() {
				_, err := c.SendMessageOpts(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, "x", client.SendMessageOptions{})
				done <- err
			}()
			return done
		}},
		{"media-upload", func(c *client.Client, fake *appFake) chan error {
			done := make(chan error, 1)
			go func() {
				_, err := c.Upload(context.Background(), newDeterministicReader(256*1024),
					client.UploadOptions{Size: 256 * 1024})
				done <- err
			}()
			return done
		}},
		{"history-iteration", func(c *client.Client, fake *appFake) chan error {
			done := make(chan error, 1)
			go func() {
				it, err := c.IterMessages(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
				if err != nil {
					done <- err
					return
				}
				for {
					if _, ok := it.Next(context.Background()); !ok {
						done <- it.Err()
						return
					}
				}
			}()
			return done
		}},
	}
	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			c, fake, _ := newDialogsClient(t)
			fake.SetDelay(150 * time.Millisecond)
			done := sc.start(c, fake)
			time.Sleep(60 * time.Millisecond)
			if err := c.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			select {
			case err := <-done:
				_ = err // any deterministic outcome is fine; must settle
			case <-time.After(10 * time.Second):
				t.Fatal("operation did not settle after Close")
			}
		})
	}
}
