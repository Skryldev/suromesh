package client_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// ---- config validation ------------------------------------------------------

func TestConfigValidation(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	cases := []struct {
		name   string
		mutate func(cfg *client.Config)
	}{
		{"negative APIID", func(c *client.Config) { c.APIID = -1 }},
		{"negative UpdateWorkers", func(c *client.Config) { c.UpdateWorkers = -2 }},
		{"negative UpdateQueue", func(c *client.Config) { c.UpdateQueue = -2 }},
		{"negative MaxPending", func(c *client.Config) { c.MaxPending = -2 }},
		{"negative RequestTimeout", func(c *client.Config) { c.RequestTimeout = -time.Second }},
		{"negative FloodWaitThreshold", func(c *client.Config) { c.FloodWaitThreshold = -time.Second }},
		{"negative Retries", func(c *client.Config) { c.Retries = -2 }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cfg := client.Config{Host: srv.Addr, TLSEnabled: boolPtr(false)}
			cfg.FillForTesting()
			tc.mutate(&cfg)
			if _, err := client.New(cfg); !errors.Is(err, client.ErrInvalidConfig) {
				t.Fatalf("error %v, want ErrInvalidConfig", err)
			}
		})
	}
}

// ---- close during in-flight operations --------------------------------------

func TestCloseDuringConnect(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	wireFakeAppServer(t, srv, testKey(9))

	c, _ := newTestClient(t, srv.Addr)
	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()

	// Let connect start, then Close concurrently.
	time.Sleep(20 * time.Millisecond)
	if err := c.Close(); err != nil && !errors.Is(err, client.ErrClosed) {
		// Close may race a completed Connect; both outcomes are valid.
		t.Logf("close: %v", err)
	}
	select {
	case err := <-connectDone:
		t.Logf("connect settled: %v (state %s)", err, c.State())
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not terminate after Close")
	}
	// Terminal guarantee: after Close the client NEVER leaves Closed —
	// regardless of whether the racing Connect finished first.
	if err := c.Connect(context.Background()); !errors.Is(err, client.ErrClosed) {
		t.Fatalf("connect after close: %v", err)
	}
	if c.State() != clientStateClosed {
		t.Fatalf("state %s, want closed", c.State())
	}
}

func TestCloseDuringInvoke(t *testing.T) {
	srv, err := wstestNew()
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
	fake.SetDelay(5 * time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := c.Invoke(context.Background(), &pingLike{})
		done <- err
	}()
	time.Sleep(100 * time.Millisecond) // request in flight
	start := time.Now()
	_ = c.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v with an in-flight invoke", elapsed)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invoke succeeded after client close")
		}
		if !errors.Is(err, client.ErrClosed) && !errors.Is(err, rpc.ErrEngineClosed) {
			t.Fatalf("invoke error %v, want closed/engine-closed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("invoke did not terminate after Close")
	}
}

func TestCloseDuringRetryBackoff(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return nil, &rpc.RPCError{Code: 500, Message: "INTERNAL"}
	})

	done := make(chan error, 1)
	go func() {
		_, err := c.Invoke(context.Background(), &pingLike{})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond) // enter 2s retry backoff
	start := time.Now()
	_ = c.Close()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v during retry backoff", elapsed)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invoke completed during client close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry backoff did not terminate after Close")
	}
}

func TestCloseDuringUpdateDelivery(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)

	var blocked int64
	block := make(chan struct{})
	c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if atomic.CompareAndSwapInt64(&blocked, 0, 1) {
			<-block // hold one worker: delivery in progress
		}
	})
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.UpdateShort{Update: &gen.UpdateConfig{}, Date: 1}, nil
	})
	if _, err := c.Invoke(context.Background(), &pingLike{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // update delivering

	start := time.Now()
	_ = c.Close()
	close(block) // release the handler
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v during update delivery", elapsed)
	}
}

func TestCloseDuringSubscriptionChurn(t *testing.T) {
	srv, err := wstestNew()
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
		return &gen.UpdateShort{Update: &gen.UpdateConfig{}, Date: 1}, nil
	})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c.OnUpdate(func(ctx context.Context, upd tl.Object) {}).Unsubscribe()
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	_ = c.Close()
	close(stop)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Close took %v during subscription churn", elapsed)
	}
	wg.Wait()
}

// ---- cancellation with extremely short deadlines ----------------------------

func TestShortDeadlineCancellation(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The default responder answers pings instantly; the short deadlines
	// race a real (fast) response — exactly the scheduling-noise case the
	// test must tolerate.

	for _, d := range []time.Duration{time.Millisecond, 5 * time.Millisecond, 50 * time.Millisecond} {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		_, err := c.Invoke(ctx, &pingLike{})
		cancel()
		if err == nil {
			continue // won the race — valid
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("deadline %v: error %v loses cancellation identity", d, err)
		}
	}
	// Client healthy afterwards.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Invoke(ctx, &pingLike{}); err != nil {
		t.Fatalf("unhealthy after short deadlines: %v", err)
	}
}

// ---- reconnect recovery + latency --------------------------------------------

func TestReconnectRecoveryAndLatency(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	latencies := make([]time.Duration, 0, 5)
	for cycle := 0; cycle < 5; cycle++ {
		if err := c.Disconnect(context.Background()); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		if err := c.Connect(context.Background()); err != nil {
			t.Fatalf("reconnect %d: %v", cycle, err)
		}
		latencies = append(latencies, time.Since(start))
		// Ready means traffic works.
		if _, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(cycle)}); err != nil {
			t.Fatalf("post-reconnect invoke %d: %v", cycle, err)
		}
	}
	var total time.Duration
	for _, d := range latencies {
		total += d
	}
	avg := total / time.Duration(len(latencies))
	t.Logf("reconnect latency avg=%v (5 cycles)", avg)
	if avg > time.Second {
		t.Fatalf("reconnect latency regressed: avg %v", avg)
	}
}

// ---- soak: 10k invokes + updates + churn + memory audit -----------------------

func TestSoakProductionLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("soak skipped in -short mode")
	}
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	const (
		invokes       = 10_000
		updateBatches = 1_000 // each carries 100 updates → 100k updates
	)

	var received int64
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt64(&received, 1)
	})
	defer sub.Unsubscribe()

	// Interleaved responder: every pingN where N%10==0 answers with a
	// 100-update batch (driven by the client's random_id marker we can
	// observe server-side via the ping payload sign).
	var counter int64
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		req, err := decodePing(body)
		if err != nil {
			return nil, &rpc.RPCError{Code: 400, Message: "BAD"}
		}
		if req.PingID < 0 {
			// Batch response: 100 updates in one Updates object.
			ups := make([]gen.UpdateClass, 100)
			for i := range ups {
				ups[i] = &gen.UpdateNewMessage{
					Message: &gen.Message{ID: int32(i), Message: "soak", PeerID: &gen.PeerUser{UserID: 1}},
					PTS:     1, PTSCount: 1,
				}
			}
			return &gen.Updates{Updates: ups, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 1, Seq: 1}, nil
		}
		atomic.AddInt64(&counter, 1)
		return &gen.Pong{PingID: req.PingID}, nil
	})

	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)
	gBefore := runtime.NumGoroutine()

	// 10k invokes in parallel; every 10th uses a negative PingID which the
	// fake answers with a 100-update batch.
	var wg sync.WaitGroup
	const workers = 20
	per := invokes / workers
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				n := w*per + i
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				if n%10 == 0 {
					if _, err := c.Invoke(ctx, &gen.PingRequest{PingID: -int64(n)}); err != nil {
						t.Errorf("batch invoke %d: %v", n, err)
					}
				} else {
					if _, err := c.Invoke(ctx, &gen.PingRequest{PingID: int64(n)}); err != nil {
						t.Errorf("invoke %d: %v", n, err)
					}
				}
				cancel()
			}
		}(w)
	}
	wg.Wait()

	// Wait for the update stream to drain (bounded wait).
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&received) >= updateBatches*100 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Memory/goroutine audit.
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)
	gAfter := runtime.NumGoroutine()

	m := c.Metrics()
	m2 := c.EngineMetrics()
	t.Logf("invokes=%d started=%d completed=%d errors=%d",
		invokes, m.InvokesStarted, m.InvokesCompleted, m.InvokeErrors)
	t.Logf("rpc breakdown: rpcErr=%d timedOut=%d cancelled=%d connLost=%d protoErr=%d pendingLimit=%d",
		m2.RequestsRPCError, m2.RequestsTimedOut, m2.RequestsCancelled, m2.RequestsConnLost, m2.ProtocolErrors, m2.PendingLimitHits)
	t.Logf("updates received=%d delivered=%d dropped=%d",
		m.UpdatesReceived, m.UpdatesDelivered, m.UpdatesDropped)
	t.Logf("goroutines before=%d after=%d; heap in use=%.1f MiB; pending=%d; cache=%d",
		gBefore, gAfter, float64(memAfter.HeapInuse)/(1<<20),
		m.PendingRequests, m.EntityCacheEntries)

	if m.PendingRequests != 0 {
		t.Fatalf("pending leak: %d", m.PendingRequests)
	}
	if m.InvokeErrors != 0 {
		// Identify the dominant failure class before failing.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, probeErr := c.Invoke(ctx, &gen.PingRequest{PingID: 1})
		t.Fatalf("invoke errors during soak: %d (probe now: %v, class %s, state %s, lastProtoErr: %s)",
			m.InvokeErrors, probeErr, client.ClassifyError(probeErr), c.State(), c.EngineMetricsDebug())
	}
	if gAfter > gBefore+16 {
		t.Fatalf("goroutine growth: %d > %d+16", gAfter, gBefore)
	}
	if m.UpdatesDropped > 0 {
		t.Logf("note: %d updates dropped by overflow policy (documented)", m.UpdatesDropped)
	}
	if memAfter.HeapInuse > 256<<20 {
		t.Fatalf("heap in use growth: %.1f MiB", float64(memAfter.HeapInuse)/(1<<20))
	}
}

func TestCacheMemoryBound(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	const total = 100_000
	for i := 0; i < total; i++ {
		c.SeedRaw(&gen.User{
			ID:         int64(i),
			AccessHash: i64p(int64(i)),
			UserType:   &gen.UserTypeNormal{},
		})
	}
	if got := c.CachedEntities(); got > 4096 {
		t.Fatalf("cache exceeded bound: %d entries", got)
	}
	// Memory associated with the cache: the map holds at most cap entries.
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	t.Logf("cache entries=%d after %d updates; heap in use=%.1f MiB",
		c.CachedEntities(), total, float64(m.HeapInuse)/(1<<20))
}
