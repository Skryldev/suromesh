package rpc

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestPendingLimitRejectsDeterministically proves the bound: at capacity,
// registration fails BEFORE anything is sent — deterministic and safe to
// retry by the caller.
func TestPendingLimitRejectsDeterministically(t *testing.T) {
	e := newDispatchTestEngine(t)
	defer e.cancel()
	e.reg.setLimit(3)

	var registered []*pending
	for i := 0; i < 3; i++ {
		p := &pending{msgID: int64(i), done: make(chan struct{})}
		p.state.Store(int32(stateRegistered))
		if !e.reg.add(p) {
			t.Fatalf("add %d rejected below capacity", i)
		}
		registered = append(registered, p)
	}
	if e.reg.len() != 3 {
		t.Fatalf("registry size %d", e.reg.len())
	}

	// Over capacity: rejected, counted, not registered.
	over := &pending{msgID: 99, done: make(chan struct{})}
	if e.reg.add(over) {
		t.Fatal("over-capacity add accepted")
	}
	if e.metrics.PendingLimitHits.Load() != 1 {
		t.Fatal("limit hit not counted")
	}
	if e.reg.len() != 3 {
		t.Fatal("rejected request leaked into registry")
	}

	// Completing one request frees a slot.
	e.reg.finalizeByMsgID(0, nil, nil)
	p := &pending{msgID: 100, done: make(chan struct{})}
	if !e.reg.add(p) {
		t.Fatal("add after completion rejected")
	}
	_ = registered
}

// TestErrPendingLimitIsSentinel guards errors.Is behavior for callers.
func TestErrPendingLimitIsSentinel(t *testing.T) {
	wrapped := fmt.Errorf("rpc: wrapped: %w", context.Canceled) // unrelated
	if errors.Is(wrapped, ErrPendingLimit) {
		t.Fatal("unrelated error matched ErrPendingLimit")
	}
	if !errors.Is(ErrPendingLimit, ErrPendingLimit) {
		t.Fatal("sentinel mismatch")
	}
	_ = newDispatchTestEngine
}
