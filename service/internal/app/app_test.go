package app_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/service/internal/app"
	"github.com/Skryldev/suromesh/service/internal/config"
)

type fakeResource struct {
	name        string
	startErr    error
	stopErr     error
	stopDelay   time.Duration
	started     bool
	stopped     bool
	mu          sync.Mutex
	onStopEnter func(*fakeResource)
}

func (f *fakeResource) Name() string { return f.name }

func (f *fakeResource) Start(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	return nil
}

func (f *fakeResource) Stop(ctx context.Context) error {
	if f.onStopEnter != nil {
		f.onStopEnter(f)
	}
	if f.stopDelay > 0 {
		select {
		case <-time.After(f.stopDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = true
	return nil
}

func (f *fakeResource) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(testWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

func testConfig() config.Config {
	cfg := config.Defaults()
	cfg.Shutdown.Timeout = 2 * time.Second
	return cfg
}

// 1. Normal startup and shutdown.
func TestRun_NormalLifecycle(t *testing.T) {
	r := &fakeResource{name: "r1"}
	a := app.New(testConfig(), testLogger(), r)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	waitFor(t, func() bool { return a.State() == app.StateRunning })
	if !r.started {
		t.Fatal("resource not started")
	}
	cancel() // operator signal path
	if err := <-done; err != nil {
		t.Fatalf("run err: %v", err)
	}
	if a.State() != app.StateStopped {
		t.Fatalf("state = %s, want stopped", a.State())
	}
	if !r.isStopped() {
		t.Fatal("resource not stopped")
	}
	_ = cancel
}

// 2. Startup failure cleans up already-created resources.
func TestRun_StartupFailureCleansUp(t *testing.T) {
	boom := errors.New("boom")
	ok1 := &fakeResource{name: "ok1"}
	bad := &fakeResource{name: "bad", startErr: boom}
	a := app.New(testConfig(), testLogger(), ok1, bad)

	err := a.Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if a.State() != app.StateFailed {
		t.Fatalf("state = %s, want failed", a.State())
	}
	if !ok1.isStopped() {
		t.Fatal("started resource not cleaned up after startup failure")
	}
}

// 3. Repeated shutdown is safe and returns the same result.
func TestStop_RepeatedAfterTerminal(t *testing.T) {
	a := app.New(testConfig(), testLogger())
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	waitFor(t, func() bool { return a.State() == app.StateRunning })
	cancel()
	<-done

	first := a.Stop(context.Background())
	second := a.Stop(context.Background())
	if first != nil || second != nil {
		t.Fatalf("stop results: %v / %v", first, second)
	}
}

// 4. Shutdown respects the context deadline (slow stop → deadline error).
func TestStop_ShutdownTimeout(t *testing.T) {
	slow := &fakeResource{name: "slow", stopDelay: 5 * time.Second}
	cfg := testConfig()
	cfg.Shutdown.Timeout = 100 * time.Millisecond
	a := app.New(cfg, testLogger(), slow)

	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(context.Background()) }()
	waitFor(t, func() bool { return a.State() == app.StateRunning })

	stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.Stop(stopCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop err = %v, want deadline exceeded", err)
	}
	if err := <-runDone; err == nil {
		t.Fatal("Run must report the shutdown failure")
	}
	if a.State() != app.StateFailed {
		t.Fatalf("state = %s, want failed", a.State())
	}
}

// 5. Concurrent Stop calls: exactly one shutdown, all callers get the
// same result.
func TestStop_ConcurrentCalls(t *testing.T) {
	var stopped int
	r := &fakeResource{name: "r", onStopEnter: func(f *fakeResource) {
		f.mu.Lock()
		stopped++
		f.mu.Unlock()
		time.Sleep(50 * time.Millisecond) // widen the race window
	}}
	a := app.New(testConfig(), testLogger(), r)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	waitFor(t, func() bool { return a.State() == app.StateRunning })

	const n = 8
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() { results <- a.Stop(context.Background()) }()
	}
	for i := 0; i < n; i++ {
		if err := <-results; err != nil {
			t.Fatalf("concurrent stop err: %v", err)
		}
	}
	<-done
	if stopped != 1 {
		t.Fatalf("resource stop entered %d times, want exactly 1", stopped)
	}
	_ = cancel
}

// 6. Resource cleanup ordering: reverse declaration order.
func TestStop_CleanupOrdering(t *testing.T) {
	var mu sync.Mutex
	var order []string
	mk := func(name string) *fakeResource {
		return &fakeResource{name: name, onStopEnter: func(f *fakeResource) {
			mu.Lock()
			order = append(order, f.name)
			mu.Unlock()
		}}
	}
	a, b, c := mk("first"), mk("second"), mk("third")
	svc := app.New(testConfig(), testLogger(), a, b, c)
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(runCtx) }()
	waitFor(t, func() bool { return svc.State() == app.StateRunning })
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "third" || order[1] != "second" || order[2] != "first" {
		t.Fatalf("cleanup order = %v, want reverse", order)
	}
	_ = cancel
}

// Additional: Stop before Run is deterministic.
func TestStop_BeforeRun(t *testing.T) {
	a := app.New(testConfig(), testLogger())
	if err := a.Stop(context.Background()); !errors.Is(err, app.ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}
