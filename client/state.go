// Package client is the production user-facing SuroMesh API: lifecycle,
// RPC invocation with reference-compatible retry semantics, updates,
// entity resolution, and messaging on top of the frozen Phase 2–5
// foundations (tl, crypto, mtproto, transport, rpc, auth, session).
//
// Concurrency: a Client is safe for concurrent use. Fixed goroutine set
// (transport 3/connection + engine dispatcher + update workers); no
// goroutine-per-request. See docs/client.md for the full model.
package client

import (
	"fmt"
	"sync"
)

// State is the client lifecycle state machine:
//
//	New → Disconnected → Connecting → Connected → Ready
//	          ▲                │            │          │
//	          │             failure       failure      │ Disconnect(ctx)
//	          └───────────────────────────────────────┘ (→ Disconnected)
//	any state ─────────────── Close() ───────────────→ Closed (terminal)
//
// Disconnect is graceful-and-reconnectable (state lands in Disconnected);
// Close is final teardown. Closing is the transient teardown phase of both.
type State int

const (
	StateNew State = iota
	StateDisconnected
	StateConnecting
	StateConnected
	StateReady
	StateClosing
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReady:
		return "ready"
	case StateClosing:
		return "closing"
	case StateClosed:
		return "closed"
	}
	return "unknown"
}

// stateMachine enforces the lifecycle: transitions are validated against a
// legality table, and Close wins races against Connect by moving through
// Closing atomically.
type stateMachine struct {
	mu    sync.Mutex
	state State
	subs  []func(from, to State)
}

func (sm *stateMachine) get() State {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state
}

func (sm *stateMachine) transition(from, to State) bool {
	sm.mu.Lock()
	if sm.state != from || !legalTransition(from, to) {
		sm.mu.Unlock()
		return false
	}
	sm.state = to
	sm.mu.Unlock()
	sm.notify(from, to)
	return true
}

// transitionAnyTo moves from any non-terminal state (used by Close via
// Closing and by terminal failure paths). Returns the previous state.
func (sm *stateMachine) transitionAnyTo(to State) (State, bool) {
	sm.mu.Lock()
	if sm.state == to {
		sm.mu.Unlock()
		return to, false
	}
	from := sm.state
	if !legalTransition(from, to) {
		// Closing is reachable from everywhere except Closed itself.
		if !(to == StateClosing && from != StateClosed) && !(from == StateClosing && to == StateClosed) {
			sm.mu.Unlock()
			return from, false
		}
	}
	sm.state = to
	sm.mu.Unlock()
	sm.notify(from, to)
	return from, true
}

func (sm *stateMachine) subscribe(fn func(from, to State)) {
	sm.mu.Lock()
	sm.subs = append(sm.subs, fn)
	sm.mu.Unlock()
}

func (sm *stateMachine) notify(from, to State) {
	sm.mu.Lock()
	subs := make([]func(State, State), len(sm.subs))
	copy(subs, sm.subs)
	sm.mu.Unlock()
	for _, fn := range subs {
		fn(from, to)
	}
}

func legalTransition(from, to State) bool {
	switch {
	case from == StateNew && to == StateDisconnected, // implicit initial
		from == StateNew && to == StateConnecting,
		from == StateDisconnected && to == StateConnecting,
		from == StateConnecting && to == StateConnected,
		from == StateConnecting && to == StateDisconnected, // failed connect
		from == StateConnected && to == StateReady,
		from == StateReady && to == StateClosing, // Disconnect path
		from == StateConnected && to == StateClosing,
		from == StateConnecting && to == StateClosing,
		from == StateClosing && to == StateDisconnected, // graceful end
		from == StateClosing && to == StateClosed,       // final end
		from == StateReady && to == StateClosing:
		return true
	}
	return false
}

var _ = fmt.Sprintf // keep fmt for future state rendering
