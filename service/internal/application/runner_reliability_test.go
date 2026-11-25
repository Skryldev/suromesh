package application_test

// AccountRunner reliability scenarios (Phase 27 §7): cancellation
// semantics, shutdown with pending work, saturation, lifecycle cycles,
// goroutine hygiene, and duplicate-runner races. Every test documents the
// exact semantic it pins — these are contract tests, not coverage noise.

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
	"github.com/Skryldev/suromesh/service/internal/testkit"
)

// TestReliability_CancelBeforeDispatch pins: a mutation whose ctx is
// already canceled never reaches the queue — the op runs zero times.
func TestReliability_CancelBeforeDispatch(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	var runs atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE dispatch
	err := r.Mutate(ctx, func(s ports.ClientSession) error {
		runs.Add(1)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if runs.Load() != 0 {
		t.Fatalf("op ran %d times, want 0 (canceled before dispatch)", runs.Load())
	}
}

// TestReliability_CancelDuringDispatch pins the documented ambiguity:
// canceling while the op executes means the caller observes ctx.Err(),
// but the op completes exactly once (the pump never drops in-flight or
// queued work). Callers needing the outcome use the idempotency ledger.
func TestReliability_CancelDuringDispatch(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	var runs atomic.Int32
	opDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Mutate(ctx, func(s ports.ClientSession) error {
			defer close(opDone)
			time.Sleep(50 * time.Millisecond) // simulate upstream latency
			runs.Add(1)
			return nil
		})
	}()
	// Cancel while the op is in flight (not merely queued).
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller not released after cancel")
	}
	<-opDone
	if runs.Load() != 1 {
		t.Fatalf("op ran %d times, want exactly 1 (in-flight work completes)", runs.Load())
	}

	// The pump remains healthy after the canceled caller walked away.
	if err := r.Mutate(context.Background(), func(s ports.ClientSession) error {
		runs.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("post-cancel mutation failed: %v", err)
	}
	if got := runs.Load(); got != 2 {
		t.Fatalf("op runs after recovery = %d, want 2", got)
	}
}

// TestReliability_ShutdownWithPendingMutations pins: Stop fails every
// queued (not yet dispatched) mutation with ErrAccountStopped — no
// silent hangs, and no executed side effects for undelivered work.
func TestReliability_ShutdownWithPendingMutations(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	block := make(chan struct{})
	var executed atomic.Int32
	headErr := make(chan error, 1)
	queuedErrs := make(chan error, runnerCfg("acc-1").MutationQueue)

	// Head blocks the pump; the rest pile up in the bounded queue.
	go func() {
		headErr <- r.Mutate(context.Background(), func(s ports.ClientSession) error {
			<-block
			executed.Add(1)
			return nil
		})
	}()
	time.Sleep(30 * time.Millisecond) // let the head enter execution
	queued := 0
	for i := 0; i < runnerCfg("acc-1").MutationQueue-1; i++ {
		go func() {
			queuedErrs <- r.Mutate(context.Background(), func(s ports.ClientSession) error {
				executed.Add(1)
				return nil
			})
		}()
		queued++
		time.Sleep(2 * time.Millisecond)
	}

	// Stop must not hang even though the head blocks forever.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	stopErr := r.Stop(ctx)
	close(block)
	if stopErr == nil {
		t.Log("stop returned nil while head mutation was in flight (bounded close path)")
	}

	// Every queued-but-undelivered mutation fails deterministically.
	for i := 0; i < queued; i++ {
		select {
		case err := <-queuedErrs:
			if err == nil {
				t.Fatalf("queued mutation %d executed after stop (want ErrAccountStopped)", i)
			}
			if !errors.Is(err, domain.ErrAccountStopped) {
				t.Fatalf("pending mutation %d err = %v, want ErrAccountStopped", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("pending mutation %d never resolved", i)
		}
	}
	// Exactly the in-flight op executed; queued ones did not.
	if got := executed.Load(); got > 1 {
		t.Fatalf("executed = %d, want ≤ 1 (only the in-flight head)", got)
	}
	// Post-stop mutations are rejected, not queued into a dead pump.
	if err := r.Mutate(context.Background(), func(s ports.ClientSession) error { return nil }); !errors.Is(err, domain.ErrAccountStopped) {
		t.Fatalf("post-stop mutate = %v, want ErrAccountStopped", err)
	}
}

// TestReliability_QueueSaturationDeterministic pins the bounded queue:
// when the pump is blocked and the queue fills, further mutations get
// ErrTooManyRequests immediately (never blocking the caller).
func TestReliability_QueueSaturationDeterministic(t *testing.T) {
	f := testkit.NewFakeFactory()
	cfg := runnerCfg("acc-1")
	cfg.MutationQueue = 4
	r := application.NewRunner(cfg, f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	release := make(chan struct{})
	go func() {
		_ = r.Mutate(context.Background(), func(s ports.ClientSession) error {
			<-release // block the pump
			return nil
		})
	}()
	time.Sleep(30 * time.Millisecond) // head is executing now

	// Submit saturating mutations ASYNCHRONOUSLY and count rejections
	// during the blocked-pump window WITHOUT waiting for completion:
	// accepted requests cannot complete until the pump is unblocked
	// (closing release), so wg.Wait() must come only after that. While
	// the pump is blocked the rejection count is monotonic — the full
	// queue stays full.
	const attempts = 64
	var wg sync.WaitGroup
	var rejected atomic.Int32
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.Mutate(context.Background(), func(s ports.ClientSession) error { return nil })
			if errors.Is(err, domain.ErrTooManyRequests) {
				rejected.Add(1)
			}
		}()
	}
	time.Sleep(300 * time.Millisecond) // settle: every submitter attempted enqueue
	if rejected.Load() == 0 {
		t.Fatal("queue never rejected: saturation policy not enforced")
	}
	t.Logf("saturation: %d/%d rejections while pump blocked (queue depth 4)",
		rejected.Load(), attempts)
	close(release) // unblock the pump: queued work completes, submitters return
	wg.Wait()
}

// TestReliability_ConcurrentMutations pins: N concurrent mutation
// requests all complete, in strictly serialized execution order.
func TestReliability_ConcurrentMutations(t *testing.T) {
	f := testkit.NewFakeFactory()
	// Queue sized to hold the whole burst: THIS test pins serialization
	// and completion of concurrent requests (saturation rejection is
	// pinned separately by TestReliability_QueueSaturationDeterministic).
	cfg := runnerCfg("acc-1")
	cfg.MutationQueue = 64
	r := application.NewRunner(cfg, f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	const n = 64
	var order []int
	var mu sync.Mutex
	var wg sync.WaitGroup
	var active, maxActive int32
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := r.Mutate(context.Background(), func(s ports.ClientSession) error {
				cur := atomic.AddInt32(&active, 1)
				if cur > maxActive {
					maxActive = cur
				}
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				atomic.AddInt32(&active, -1)
				return nil
			})
			if err != nil {
				t.Errorf("mutation %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("max concurrent mutations = %d, want 1 (serialized)", maxActive)
	}
	if len(order) != n {
		t.Fatalf("completed = %d, want %d", len(order), n)
	}
}

// TestReliability_DuplicateStartRunnerRace pins the Phase 27 fix: N
// concurrent StartRunner calls for one account construct exactly ONE
// upstream client; losers get ErrAlreadyExists (or share the start
// error) — never a second live client for the same account.
func TestReliability_DuplicateStartRunnerRace(t *testing.T) {
	f := testkit.NewFakeFactory()
	m := application.NewManager(f, &testkit.FakeVault{}, testkit.NewFakeSessions(), nil,
		application.ManagerConfig{})
	defer m.StopAll(context.Background())

	const n = 16
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = m.StartRunner(context.Background(), runnerCfg("acc-race"))
		}(i)
	}
	wg.Wait()

	winners := 0
	for i, err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("goroutine %d: unexpected error %v (want nil or ErrAlreadyExists)", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if got := f.ClientCount(); got != 1 {
		t.Fatalf("upstream clients constructed = %d, want 1 (no double-create)", got)
	}
	if m.Count() != 1 {
		t.Fatalf("registered runners = %d, want 1", m.Count())
	}
}

// TestReliability_StartFailureClosesClient pins the Phase 27 leak fix:
// when Start fails after client construction (AutoConnect failure), the
// constructed client is closed — a retry must not stack a second client
// on top of a leaked one.
func TestReliability_StartFailureClosesClient(t *testing.T) {
	f := testkit.NewFakeFactory()
	cfg := runnerCfg("acc-1")
	cfg.AutoConnect = true
	for attempt := 1; attempt <= 3; attempt++ {
		c := &testkit.FakeClient{}
		c.OnConnect = func() error { return errors.New("upstream unreachable") }
		f.OnCreate = func(spec ports.ClientSpec) (ports.ClientSession, error) { return c, nil }
		r := application.NewRunner(cfg, f, &testkit.FakeVault{}, testkit.NewFakeSessions())
		err := r.Start(context.Background())
		if err == nil {
			t.Fatalf("attempt %d: start unexpectedly succeeded", attempt)
		}
		if !c.Closed() {
			t.Fatalf("attempt %d: constructed client leaked (not closed after failed Start)", attempt)
		}
	}
	if got := f.ClientCount(); got != 3 {
		t.Fatalf("clients created = %d, want 3 (one per attempt)", got)
	}
}

// TestReliability_RepeatedStartStopCycles pins: stop→start cycles via
// the Manager always produce exactly one fresh client per cycle, every
// cycle reaches a live client, and re-registration is deterministic.
func TestReliability_RepeatedStartStopCycles(t *testing.T) {
	f := testkit.NewFakeFactory()
	m := application.NewManager(f, &testkit.FakeVault{}, testkit.NewFakeSessions(), nil,
		application.ManagerConfig{})
	defer m.StopAll(context.Background())

	const cycles = 5
	for c := 0; c < cycles; c++ {
		r, err := m.StartRunner(context.Background(), runnerCfg("acc-cycle"))
		if err != nil {
			t.Fatalf("cycle %d: start: %v", c, err)
		}
		if err := r.Mutate(context.Background(), func(s ports.ClientSession) error {
			return s.MarkAsRead(context.Background(), domain.PeerRef{}, 0)
		}); !errors.Is(err, testkit.ErrUnscripted) {
			// The fake rejects unscripted ops — proves the mutation path
			// reaches a live client every cycle.
			t.Fatalf("cycle %d: mutate = %v, want ErrUnscripted", c, err)
		}
		if err := m.StopRunner(context.Background(), "tenant-1", domain.AccountID("acc-cycle")); err != nil {
			t.Fatalf("cycle %d: stop: %v", c, err)
		}
	}
	if got := f.ClientCount(); got != cycles {
		t.Fatalf("clients created = %d, want %d (one per cycle)", got, cycles)
	}
}

// TestReliability_RepeatedConnectDisconnectCycles pins: toggling desired
// state repeatedly never wedges the runner and the observation tracks
// the last transition.
func TestReliability_RepeatedConnectDisconnectCycles(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	for i := 0; i < 10; i++ {
		if err := r.SetDesired(context.Background(), domain.DesiredRunning); err != nil {
			t.Fatalf("cycle %d: connect: %v", i, err)
		}
		if !r.SessionConnected() {
			t.Fatalf("cycle %d: not connected after connect", i)
		}
		if err := r.SetDesired(context.Background(), domain.DesiredStopped); err != nil {
			t.Fatalf("cycle %d: disconnect: %v", i, err)
		}
		if r.SessionConnected() {
			t.Fatalf("cycle %d: still connected after disconnect", i)
		}
	}
	obs := r.Observation()
	if obs.Desired != domain.DesiredStopped {
		t.Fatalf("desired = %s, want stopped", obs.Desired)
	}
}

// TestReliability_GoroutineHygiene pins: start/stop/subscribe/unsubscribe
// cycles do not accumulate goroutines. The fake client's teardown (Close
// closes its update channel) mirrors the real adapter's Close contract;
// the SDK adapter closes its update pump on Close.
func TestReliability_GoroutineHygiene(t *testing.T) {
	f := testkit.NewFakeFactory()
	m := application.NewManager(f, &testkit.FakeVault{}, testkit.NewFakeSessions(), nil,
		application.ManagerConfig{})
	defer m.StopAll(context.Background())

	cycle := func() {
		r, err := m.StartRunner(context.Background(), runnerCfg("acc-leak"))
		if err != nil {
			t.Fatal(err)
		}
		ch, cancel, err := r.Subscribe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		_ = ch
		if err := m.StopRunner(context.Background(), "tenant-1", domain.AccountID("acc-leak")); err != nil {
			t.Fatal(err)
		}
	}
	// Warm-up (compaction and test-framework goroutines settle).
	for i := 0; i < 3; i++ {
		cycle()
	}
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		cycle()
	}
	// Goroutine teardown is asynchronous: poll with a budget instead of
	// asserting an exact instant.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 { // tolerance for runtime noise
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: before=%d after=%d (20 cycles)", before, runtime.NumGoroutine())
}

// TestReliability_SubscriberCleanupOnRunnerStop pins: all subscribers are
// released (channels closed) when the runner stops, and subscribe after
// stop fails deterministically.
func TestReliability_SubscriberCleanupOnRunnerStop(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	const n = 8
	chans := make([]<-chan domain.UpdateEvent, n)
	for i := 0; i < n; i++ {
		ch, cancel, err := r.Subscribe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		chans[i] = ch
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, ch := range chans {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("subscriber %d: channel still open after runner stop", i)
			}
		case <-time.After(1 * time.Second):
			t.Fatalf("subscriber %d: channel not closed after runner stop", i)
		}
	}
	if _, _, err := r.Subscribe(context.Background()); !errors.Is(err, domain.ErrAccountStopped) {
		t.Fatalf("subscribe after stop = %v, want ErrAccountStopped", err)
	}
}
