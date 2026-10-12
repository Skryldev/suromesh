package transport

import (
	"fmt"
	"sync"
)

// State is the transport lifecycle state machine:
//
//	 New ──Connect()──▶ Connecting ──dial ok──▶ Connected
//	                     │  ▲                    │ │
//	        dial fail,    │  └──── conn lost ─────┘ │
//	        budget left  └───▶ Reconnecting ◀───────┘
//	                          │   │
//	            dial ok ─────┘   └─ budget exhausted / fatal ─▶ Closed
//	any state ────────────── Close() ─────────────────────▶ Closed
//
// Illegal transitions (Connected → Connecting, anything → New, anything
// except idempotent Close out of Closed) are rejected with ErrBadState.
type State int

const (
	StateNew State = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateNew:
		return "new"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateClosed:
		return "closed"
	}
	return "unknown"
}

// stateMachine guards transitions; the mutex is only held for compare-and-
// set style transitions, never across I/O. subs is the Phase 4 subscription
// point (Transport.Subscribe).
type stateMachine struct {
	mu     sync.Mutex
	state  State
	onDiff func(from, to State)
	subs   []func(from, to State)
}

func (sm *stateMachine) notify(from, to State) {
	// Callbacks run OUTSIDE the state lock: they legitimately call back
	// into the transport (e.g. the request engine's Send → State()), which
	// would self-deadlock under the lock. Snapshot, release, then invoke.
	sm.mu.Lock()
	onDiff := sm.onDiff
	subs := make([]func(from, to State), len(sm.subs))
	copy(subs, sm.subs)
	sm.mu.Unlock()

	if onDiff != nil {
		onDiff(from, to)
	}
	for _, fn := range subs {
		fn(from, to)
	}
}

func (sm *stateMachine) get() State {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.state
}

// transition attempts from→to; illegal transitions are rejected with the
// current state and false, making invalid lifecycle moves impossible.
// Observers are invoked after the state lock is released (notify must not
// run under the lock: callbacks may call back into the transport).
func (sm *stateMachine) transition(from, to State) (State, bool) {
	sm.mu.Lock()
	if sm.state != from || legalTransition(from, to) != nil {
		cur := sm.state
		sm.mu.Unlock()
		return cur, false
	}
	sm.state = to
	sm.mu.Unlock()
	sm.notify(from, to)
	return to, true
}

// transitionAny moves from any state except `to` itself; used for terminal
// transitions where the source does not matter.
func (sm *stateMachine) transitionAny(to State) (State, bool) {
	sm.mu.Lock()
	if sm.state == to {
		sm.mu.Unlock()
		return to, false
	}
	from := sm.state
	sm.state = to
	sm.mu.Unlock()
	sm.notify(from, to)
	return to, true
}

// legalTransition is a pure table check used by tests and documentation.
func legalTransition(from, to State) error {
	switch {
	case from == StateNew && to == StateConnecting,
		from == StateConnecting && to == StateConnected,
		from == StateConnecting && to == StateReconnecting,
		from == StateConnecting && to == StateClosed,
		from == StateConnected && to == StateReconnecting,
		from == StateConnected && to == StateClosed,
		from == StateReconnecting && to == StateConnected,
		from == StateReconnecting && to == StateReconnecting, // retry iteration
		from == StateReconnecting && to == StateClosed,
		from == StateNew && to == StateClosed:
		return nil
	}
	return fmt.Errorf("%w: %s → %s", ErrBadState, from, to)
}
