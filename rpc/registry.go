package rpc

import (
	"sync"
	"sync/atomic"

	"github.com/Skryldev/suromesh/tl"
)

// Request lifecycle states (atomic CAS transitions; exactly one terminal):
//
//	Created → Registered → Sent → AwaitingResponse → Completed
//	                                    ├→ RPCError
//	                                    ├→ Timeout      (caller ctx)
//	                                    ├→ Cancelled    (caller ctx)
//	                                    └→ ConnectionLost
//	Registered/Sent → Cancelled (caller ctx before/while sending)
//
// Every path reaches a terminal state: the dispatcher, the canceller, or
// engine shutdown finalizes exactly once, and the winner removes the entry
// from the registry — a completed request is unreachable from the registry.
type reqState int32

const (
	stateCreated reqState = iota
	stateRegistered
	stateSent
	stateAwaiting
	stateTerminal
)

func (s reqState) String() string {
	switch s {
	case stateCreated:
		return "created"
	case stateRegistered:
		return "registered"
	case stateSent:
		return "sent"
	case stateAwaiting:
		return "awaiting"
	case stateTerminal:
		return "terminal"
	}
	return "unknown"
}

// pending is one in-flight request. The registry map holds it until the
// single terminal transition removes it.
type pending struct {
	msgID int64
	seqNo int32

	state atomic.Int32

	// Completion: done is closed exactly once by the terminal winner; the
	// outcome fields are written before close and read after.
	done   chan struct{}
	once   sync.Once
	result tl.Object
	err    error

	// body is the serialized request (kept for resend policies such as
	// bad-server-salt recovery when configured).
	body           []byte
	contentRelated bool
}

// terminal transitions the request to its final state; returns false when
// something else (response, timeout, shutdown) won the race. The winner
// records the outcome and closes done; registry removal is the caller's
// (winning) side effect — via registry.finalize.
func (p *pending) finalize(result tl.Object, err error) bool {
	if !p.state.CompareAndSwap(int32(stateAwaiting), int32(stateTerminal)) &&
		!p.state.CompareAndSwap(int32(stateSent), int32(stateTerminal)) &&
		!p.state.CompareAndSwap(int32(stateRegistered), int32(stateTerminal)) {
		return false
	}
	p.result = result
	p.err = err
	p.once.Do(func() { close(p.done) })
	return true
}

// wait closed-ch check.
func (p *pending) isDone() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// registry is the pending-request table keyed by message ID.
//
// Concurrency: a single RWMutex over a plain map. Ops are O(1) with tiny
// critical sections; the dispatcher (one goroutine) does lookups/removals
// while caller goroutines insert and cancel. Benchmarks in bench_test.go
// gate any future sharding decision — no sharding without evidence.
type registry struct {
	mu      sync.RWMutex
	items   map[int64]*pending
	limit   int // max pending entries; 0 = unbounded
	metrics *Metrics
}

func newRegistry(m *Metrics) *registry {
	return &registry{items: make(map[int64]*pending), metrics: m}
}

// add registers a freshly created request. The msg_id must be unique (the
// id assigner guarantees monotonic uniqueness). It returns false when the
// registry is at capacity — the request is NOT registered and must fail
// with ErrPendingLimit (deterministic; nothing was sent).
func (r *registry) add(p *pending) bool {
	r.mu.Lock()
	if r.limit > 0 && len(r.items) >= r.limit {
		r.mu.Unlock()
		r.metrics.PendingLimitHits.Add(1)
		return false
	}
	r.items[p.msgID] = p
	r.mu.Unlock()
	r.metrics.RequestsStarted.Add(1)
	return true
}

// setLimit configures the pending capacity (<=0 disables the bound).
func (r *registry) setLimit(n int) {
	r.mu.Lock()
	r.limit = n
	r.mu.Unlock()
}

// limit reports the configured capacity (0 = unbounded).
func (r *registry) pendingLimit() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.limit
}

// lookup returns the pending request without removing it.
func (r *registry) lookup(msgID int64) (*pending, bool) {
	r.mu.RLock()
	p, ok := r.items[msgID]
	r.mu.RUnlock()
	return p, ok
}

// remove drops the entry unconditionally (idempotent).
func (r *registry) remove(msgID int64) {
	r.mu.Lock()
	delete(r.items, msgID)
	r.mu.Unlock()
}

// len reports the current pending count (diagnostics/tests).
func (r *registry) len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}

// finalizeByMsgID completes the request registered under msgID if present
// and still pending. Returns the call and whether this caller won the
// terminal race. Unknown/late/duplicate responses are classified by the
// return value.
func (r *registry) finalizeByMsgID(msgID int64, result tl.Object, err error) (*pending, bool) {
	p, ok := r.lookup(msgID)
	if !ok {
		r.metrics.UnknownResponses.Add(1)
		return nil, false
	}
	if !p.finalize(result, err) {
		r.metrics.DuplicateResponses.Add(1)
		return p, false
	}
	r.remove(msgID)
	r.metrics.RequestsCompleted.Add(1)
	return p, true
}

// failAll deterministically fails every pending request (connection loss,
// engine shutdown). It is a single terminal cleanup path for all entries;
// no request remains registered afterwards.
func (r *registry) failAll(err error) int {
	r.mu.Lock()
	items := r.items
	r.items = make(map[int64]*pending, len(items)/2)
	r.mu.Unlock()

	n := 0
	for _, p := range items {
		if p.finalize(nil, err) {
			n++
			switch {
			case errorsIs(err, ErrConnectionLost):
				r.metrics.RequestsConnLost.Add(1)
			default:
				r.metrics.RequestsCancelled.Add(1)
			}
		}
	}
	return n
}

// resendables snapshots the pending entries (bad-server-salt recovery).
func (r *registry) snapshot() []*pending {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*pending, 0, len(r.items))
	for _, p := range r.items {
		out = append(out, p)
	}
	return out
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
