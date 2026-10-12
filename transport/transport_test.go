package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/transport/abridged"
	"github.com/Skryldev/suromesh/transport/ws"
)

func TestStateMachineLegalTransitions(t *testing.T) {
	for _, tr := range [][2]State{
		{StateNew, StateConnecting},
		{StateConnecting, StateConnected},
		{StateConnecting, StateReconnecting},
		{StateConnecting, StateClosed},
		{StateConnected, StateReconnecting},
		{StateConnected, StateClosed},
		{StateReconnecting, StateConnected},
		{StateReconnecting, StateReconnecting},
		{StateReconnecting, StateClosed},
		{StateNew, StateClosed},
	} {
		if err := legalTransition(tr[0], tr[1]); err != nil {
			t.Fatalf("%s → %s should be legal: %v", tr[0], tr[1], err)
		}
	}
	for _, tr := range [][2]State{
		{StateNew, StateConnected},        // skip dial
		{StateConnected, StateConnecting}, // re-dial without recycle
		{StateClosed, StateConnecting},    // resurrection
		{StateClosed, StateConnected},
		{StateNew, StateReconnecting},
	} {
		if err := legalTransition(tr[0], tr[1]); err == nil {
			t.Fatalf("%s → %s should be illegal", tr[0], tr[1])
		}
	}
}

func TestStateMachineRejectsInvalidTransitions(t *testing.T) {
	sm := &stateMachine{}
	if _, ok := sm.transition(StateNew, StateConnected); ok {
		t.Fatal("illegal transition accepted")
	}
	if sm.get() != StateNew {
		t.Fatal("state changed on rejected transition")
	}
	if _, ok := sm.transition(StateNew, StateConnecting); !ok {
		t.Fatal("legal transition rejected")
	}
	// Closed is terminal: only transitionAny reaches it, and nothing leaves it.
	sm.transitionAny(StateClosed)
	if _, ok := sm.transition(StateClosed, StateConnected); ok {
		t.Fatal("transport resurrected from Closed")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		err  error
		want Classification
	}{
		{context.Canceled, ClassCancelled},
		{ErrQueueFull, ClassResource},
		{fmt.Errorf("wrap: %w", abridged.ErrTooLarge), ClassProtocol},
		{fmt.Errorf("wrap: %w", ws.ErrProtocol), ClassProtocol},
		{fmt.Errorf("wrap: %w", ws.ErrTooBig), ClassProtocol},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, ClassTemporary},
		{io.EOF, ClassClosed},
		{ErrClosed, ClassClosed},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Fatalf("Classify(%v) = %s, want %s", c.err, got, c.want)
		}
	}
	// Timeout errors via net.Error.
	if got := Classify(&net.OpError{Op: "read", Err: &timeoutError{}}); got != ClassTimeout {
		t.Fatalf("timeout classified %s", got)
	}
}

type timeoutError struct{}

func (e *timeoutError) Error() string   { return "i/o timeout" }
func (e *timeoutError) Timeout() bool   { return true }
func (e *timeoutError) Temporary() bool { return false }

func TestReconnectableClasses(t *testing.T) {
	if !ClassTemporary.reconnectable() || !ClassTimeout.reconnectable() || !ClassClosed.reconnectable() {
		t.Fatal("temporary/timeout/closed must reconnect")
	}
	if ClassProtocol.reconnectable() || ClassResource.reconnectable() || ClassCancelled.reconnectable() {
		t.Fatal("protocol/resource/cancelled must not reconnect")
	}
}

func TestBackoffBounds(t *testing.T) {
	tr := &Transport{cfg: Config{BackoffBase: 10 * time.Millisecond, BackoffMax: 50 * time.Millisecond, BackoffJitter: 0}}
	tr.ctx, tr.cancel = context.WithCancel(context.Background())
	defer tr.cancel()

	// Grows toward the cap; timer variance makes strict monotonicity
	// flaky, so assert the cap and a lower bound instead.
	for attempt := 1; attempt <= 10; attempt++ {
		start := time.Now()
		if !tr.sleepBackoff(attempt) {
			t.Fatal("backoff sleep cancelled unexpectedly")
		}
		d := time.Since(start)
		want := 10 * time.Millisecond << uint(min(attempt-1, 16))
		if want > 50*time.Millisecond {
			want = 50 * time.Millisecond
		}
		if d < want*9/10 {
			t.Fatalf("attempt %d: slept %v, want ≈%v", attempt, d, want)
		}
		if d > want+250*time.Millisecond {
			t.Fatalf("attempt %d: slept %v, want ≈%v", attempt, d, want)
		}
	}
}

func TestBackoffCancellation(t *testing.T) {
	tr := &Transport{cfg: Config{BackoffBase: time.Hour, BackoffMax: time.Hour, BackoffJitter: 0}}
	tr.ctx, tr.cancel = context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- tr.sleepBackoff(1) }()
	time.Sleep(50 * time.Millisecond)
	tr.cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("backoff completed despite cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("backoff did not react to cancellation")
	}
}

func TestSendRecvBeforeConnect(t *testing.T) {
	tr := New(Config{Host: "127.0.0.1:1", PingInterval: -1})
	defer tr.Close()
	if err := tr.Send(context.Background(), []byte{1, 2, 3, 4}); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("send before connect: %v", err)
	}
	// Recv with a cancelled context returns promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tr.Recv(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("recv: %v", err)
	}
}

func TestCloseIsIdempotentAndTerminal(t *testing.T) {
	tr := New(Config{Host: "127.0.0.1:1", PingInterval: -1, MaxReconnects: 0, BackoffBase: time.Millisecond})
	_ = tr.Connect(context.Background()) // fails fast: budget 0
	if tr.State() != StateClosed {
		t.Fatalf("state after exhaustion: %s", tr.State())
	}
	if err := tr.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second close: %v", err)
	}
	select {
	case <-tr.Done():
	default:
		t.Fatal("Done not closed after terminal failure")
	}
}

// Compile-time interface checks for the entropy indirection.
var _ io.Reader = (*countingReader)(nil)

type countingReader struct{ n int }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(c.n + i)
	}
	c.n++
	return len(p), nil
}
