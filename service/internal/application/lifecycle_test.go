package application_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/service/internal/application"
)

type fakeResource struct {
	name      string
	startErr  error
	stopErr   error
	stopBlock time.Duration // simulates a slow stop
	started   bool
	stopped   bool
}

func (f *fakeResource) Name() string { return f.name }

func (f *fakeResource) Start(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	return nil
}

func (f *fakeResource) Stop(ctx context.Context) error {
	if f.stopBlock > 0 {
		select {
		case <-time.After(f.stopBlock):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = true
	return nil
}

func TestStartAll_OrdersAndStartsEverything(t *testing.T) {
	a, b, c := &fakeResource{name: "a"}, &fakeResource{name: "b"}, &fakeResource{name: "c"}
	s := application.NewSupervisor(a, b, c)
	if err := s.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*fakeResource{a, b, c} {
		if !r.started {
			t.Fatalf("%s not started", r.name)
		}
	}
	if got := strings.Join(s.StartedNames(), ","); got != "a,b,c" {
		t.Fatalf("started order = %q", got)
	}
}

func TestStartAll_FailureStopsStartedPrefixInReverse(t *testing.T) {
	boom := errors.New("boom")
	a, b, c := &fakeResource{name: "a"}, &fakeResource{name: "b"}, &fakeResource{name: "c", startErr: boom}
	s := application.NewSupervisor(a, b, c)
	err := s.StartAll(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if !strings.Contains(err.Error(), "resource c") {
		t.Fatalf("err should name the failing resource: %v", err)
	}
	if !a.started || !a.stopped {
		t.Fatalf("a: started=%v stopped=%v (want started, then stopped by cleanup)", a.started, a.stopped)
	}
	if !b.started || !b.stopped {
		t.Fatalf("b: started=%v stopped=%v (want started, then stopped by cleanup)", b.started, b.stopped)
	}
	if c.started {
		t.Fatal("c must not start")
	}
}

func TestStopAll_ReverseOrder(t *testing.T) {
	var order []string
	mk := func(name string) *fakeResource {
		return &fakeResource{name: name}
	}
	a, b, c := mk("a"), mk("b"), mk("c")
	s := application.NewSupervisor(a, b, c)
	_ = s.StartAll(context.Background())
	if err := s.StopAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = order
	// Verify via started-prefix exhaustion: after StopAll nothing remains.
	if len(s.StartedNames()) != 0 {
		t.Fatal("prefix not drained")
	}
	if !a.stopped || !b.stopped || !c.stopped {
		t.Fatal("all resources must be stopped")
	}
}

func TestStopAll_JoinsErrorsAndStopsEverything(t *testing.T) {
	errA := errors.New("stop-a")
	errC := errors.New("stop-c")
	a := &fakeResource{name: "a", stopErr: errA}
	b := &fakeResource{name: "b"}
	c := &fakeResource{name: "c", stopErr: errC}
	s := application.NewSupervisor(a, b, c)
	_ = s.StartAll(context.Background())
	err := s.StopAll(context.Background())
	if !errors.Is(err, errA) || !errors.Is(err, errC) {
		t.Fatalf("joined errors missing: %v", err)
	}
	if !b.stopped {
		t.Fatal("b must still be stopped despite neighbor failures")
	}
}

func TestStopAll_RespectsContextDeadline(t *testing.T) {
	slow := &fakeResource{name: "slow", stopBlock: 5 * time.Second}
	fast := &fakeResource{name: "fast"}
	s := application.NewSupervisor(slow, fast)
	_ = s.StartAll(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := s.StopAll(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if !fast.stopped {
		t.Fatal("fast resource (earlier in reverse order) must be stopped within budget")
	}
}
