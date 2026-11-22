package application_test

// Layer D (§4.3) + Layer I (§9): AccountRunner/Manager concurrency stress
// and benchmarks. Deterministic; no upstream; exercises real queue,
// semaphore, hub, and state-machine paths.

import (
	"context"
	"errors"
	"fmt"
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

type portsClientSession = ports.ClientSession

// repeatedRunner builds + starts a runner with the given queue sizing.
func repeatedRunner(t *testing.T, account string, q int) (*application.Runner, func()) {
	t.Helper()
	r := application.NewRunner(application.RunnerConfig{
		AccountID: domain.AccountID(account), TenantID: "t",
		MutationQueue: q, ReadConcurrent: 4, StreamBuffer: 4,
	}, testkit.NewFakeFactory(), &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r, func() { _ = r.Stop(context.Background()) }
}

// TestStress_MutationThroughputAndOrdering: 200 mutations through the
// serialized queue — all complete, none lost, single-consumer guarantee
// exercised under load.
func TestStress_MutationThroughputAndOrdering(t *testing.T) {
	r, stop := repeatedRunner(t, "stress-m", 200)
	defer stop()

	var counter int64
	var mu sync.Mutex
	var order []int64
	for i := 0; i < 200; i++ {
		if err := r.Mutate(context.Background(), func(s portsSess) error {
			mu.Lock()
			order = append(order, int64(i))
			mu.Unlock()
			atomic.AddInt64(&counter, 1)
			return nil
		}); err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	if atomic.LoadInt64(&counter) != 200 {
		t.Fatalf("completed = %d, want 200", counter)
	}
	for i, v := range order {
		if v != int64(i) {
			t.Fatalf("ordering violated at %d: %v", i, order)
		}
	}
}

// portsSess aliases the session port for the stress closures.
type portsSess = portsClientSession

// TestStress_QueueSaturationRejectsDeterministically: queue depth 2; the
// pump is blocked; submissions beyond capacity fail with
// ErrTooManyRequests — never block forever.
func TestStress_QueueSaturationRejectsDeterministically(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(application.RunnerConfig{
		AccountID: "sat", TenantID: "t", MutationQueue: 2,
		ReadConcurrent: 4, StreamBuffer: 4,
	}, f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	blockErr := errors.New("blocked")
	go func() {
		_ = r.Mutate(context.Background(), func(s portsSess) error {
			<-release
			return blockErr
		})
	}()
	time.Sleep(30 * time.Millisecond) // let the blocker occupy the pump

	rejected := 0
	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := r.Mutate(ctx, func(s portsSess) error { return nil })
		cancel()
		if errors.Is(err, domain.ErrTooManyRequests) {
			rejected++
		}
	}
	close(release)
	if rejected == 0 {
		t.Fatal("no submissions rejected under saturation (queue unbounded?)")
	}
	t.Logf("saturated queue rejected %d/10 submissions", rejected)
}

// TestStress_ConcurrentSubscribeUnsubscribe_NoLeak: churn 200 subscribe/
// unsubscribe cycles while events flow; goroutine count must return to
// baseline and the hub must not panic.
func TestStress_ConcurrentSubscribeUnsubscribe(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("hub-churn"), f, &testkit.FakeVault{},
		testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	client := f.ClientFor(domain.AccountID("hub-churn"))
	before := runtime.NumGoroutine()

	var wg sync.WaitGroup
	stopFlag := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				ch, cancel, err := r.Subscribe(context.Background())
				if err != nil {
					return
				}
				_ = ch
				if j%2 == 0 {
					cancel()
				} else {
					time.AfterFunc(2*time.Millisecond, cancel)
				}
				select {
				case <-stopFlag:
					return
				default:
				}
			}
		}(i)
	}
	// Publish continuously during the churn.
	go func() {
		for k := 0; k < 200; k++ {
			client.Publish(domain.UpdateEvent{Kind: domain.UpdateNewMessage,
				NewMessage: &domain.Message{ID: int64(k + 1), Text: "x"}})
			time.Sleep(time.Millisecond)
		}
		close(stopFlag)
	}()
	wg.Wait()

	// Allow deferred cancels to land; then verify no goroutine explosion.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+20 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	after := runtime.NumGoroutine()
	if after > before+20 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

// TestStress_MultipleSubscribersAllReceive: 5 subscribers each receive
// every published event (no cross-subscriber starvation).
func TestStress_MultipleSubscribersAllReceive(t *testing.T) {
	const subs, events = 5, 20
	f := testkit.NewFakeFactory()
	// Buffer >= events: this test verifies fan-out completeness for ACTIVE
	// consumers (overflow/drop behavior is covered separately by
	// TestRunner_SlowSubscriberDropped).
	cfg := runnerCfg("multi-sub")
	cfg.StreamBuffer = events + 10
	r := application.NewRunner(cfg, f, &testkit.FakeVault{},
		testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	chans := make([]<-chan domain.UpdateEvent, subs)
	cancels := make([]func(), subs)
	for i := range chans {
		ch, cancel, err := r.Subscribe(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		chans[i], cancels[i] = ch, cancel
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	client := f.ClientFor(domain.AccountID("multi-sub"))
	for i := 0; i < events; i++ {
		client.Publish(domain.UpdateEvent{Kind: domain.UpdateNewMessage,
			NewMessage: &domain.Message{ID: int64(i + 1), Text: "e"}})
	}

	received := make([]int, subs)
	var wg sync.WaitGroup
	for i := range chans {
		wg.Add(1)
		go func(i int, ch <-chan domain.UpdateEvent) {
			defer wg.Done()
			for n := 0; n < events; n++ {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
					received[i]++
				case <-time.After(2 * time.Second):
					return
				}
			}
		}(i, chans[i])
	}
	wg.Wait()
	for i, n := range received {
		if n != events {
			t.Fatalf("subscriber %d received %d/%d events", i, n, events)
		}
	}
}

// TestStress_StopAllIdempotentAndComplete: repeated StopAll never
// deadlocks and leaves zero runners.
func TestStress_StopAllIdempotent(t *testing.T) {
	f := testkit.NewFakeFactory()
	m := application.NewManager(f, &testkit.FakeVault{}, testkit.NewFakeSessions(), nil,
		application.ManagerConfig{})
	for i := 0; i < 5; i++ {
		if _, err := m.StartRunner(context.Background(),
			application.RunnerConfig{AccountID: domain.AccountID(fmt.Sprintf("a%d", i)), TenantID: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.StopAll(ctx); err != nil { // repeated: no-op, no deadlock
		t.Fatal(err)
	}
	if m.Count() != 0 {
		t.Fatalf("count after StopAll = %d", m.Count())
	}
}

// TestStress_RunnerStopTwice: Stop is idempotent per runner.
func TestStress_RunnerStopTwice(t *testing.T) {
	r, _ := repeatedRunner(t, "stop2", 8)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

// ---- benchmarks (Layer I) ---------------------------------------------------

func BenchmarkMutationDispatch(b *testing.B) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(application.RunnerConfig{
		AccountID: "bench", TenantID: "t", MutationQueue: b.N + 10,
		ReadConcurrent: 8, StreamBuffer: 8,
	}, f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer r.Stop(context.Background())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Mutate(context.Background(), func(s portsSess) error { return nil })
	}
}

func BenchmarkReadDispatch(b *testing.B) {
	r, stop := repeatedRunnerB(b, "bench-read")
	defer stop()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = r.Read(context.Background(), func(s portsSess) error { return nil })
		}
	})
}

func repeatedRunnerB(b *testing.B, account string) (*application.Runner, func()) {
	r := application.NewRunner(application.RunnerConfig{
		AccountID: domain.AccountID(account), TenantID: "t",
		MutationQueue: 1000000, ReadConcurrent: 32, StreamBuffer: 8,
	}, testkit.NewFakeFactory(), &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	return r, func() { _ = r.Stop(context.Background()) }
}

func BenchmarkUpdateHubPublish(b *testing.B) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("hub-bench"), f, &testkit.FakeVault{},
		testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer r.Stop(context.Background())
	// 5 active consumers draining continuously.
	for i := 0; i < 5; i++ {
		ch, cancel, _ := r.Subscribe(context.Background())
		go func(ch <-chan domain.UpdateEvent, cancel func()) {
			for range ch {
			}
			_ = cancel
		}(ch, cancel)
	}
	client := f.ClientFor(domain.AccountID("hub-bench"))
	ev := domain.UpdateEvent{Kind: domain.UpdateNewMessage,
		NewMessage: &domain.Message{ID: 1, Text: "bench"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		client.Publish(ev)
	}
	b.StopTimer()
}
