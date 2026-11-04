package client

import (
	"time"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/transport"
)

// Hooks are optional observability callbacks. All hooks must be
// non-blocking: they fire on lifecycle and RPC paths, and a slow hook
// delays the operation that produced the event. No hook ever receives
// secret material (auth keys, nonces); objects and identifiers only.
//
// The library has no mandatory logging framework: adapt slog/zap/zerolog
// by implementing these callbacks.
type Hooks struct {
	// OnStateChange fires on every client lifecycle transition.
	OnStateChange func(from, to State)
	// OnAuthenticated fires when a connection is established with a valid
	// auth key (freshly exchanged or reused from the session).
	OnAuthenticated func()
	// OnRPCStart fires when an Invoke begins (after guards).
	OnRPCStart func(method string)
	// OnRPCComplete fires when an Invoke settles (success or final error).
	OnRPCComplete func(method string, dur time.Duration, err error)
	// OnRPCRetry fires on each automatic retry with its classification.
	OnRPCRetry func(method string, attempt int, reason error)
	// OnUpdateReceived fires for every update entering the subsystem
	// (before handler dispatch).
	OnUpdateReceived func(ctorID uint32)
	// OnUpdateGap fires when the update engine detects a gap or an
	// unsupported update family (kind is "gap" or "unsupported family").
	// No payload, no identifiers — the kind only. Panic isolated.
	OnUpdateGap func(kind string)
	// OnDifferenceSync fires when a difference request completes or fails
	// ("account" or "channel" scope; failures are visible through
	// Metrics). No payload. Panic isolated.
	OnDifferenceSync func(scope string)
	// OnSlowConsumer fires when the update queue is full and an update is
	// being dropped (the explicit overflow policy; see updateManager).
	OnSlowConsumer func(queueLen, droppedTotal int)
	// OnSubscriberPanic fires when an update handler panics (no recovery
	// payload — it may contain user content).
	OnSubscriberPanic func()
	// OnTransportError fires for classified transport errors.
	OnTransportError func(err error, class transport.Classification)
}

// methodName derives a stable identifier for hook reporting.
func methodName(req tl.Object) string {
	if req == nil {
		return "?"
	}
	return ctorName(req.CtorID())
}

var ctorNames = map[uint32]string{
	0xda9b0d0d: "invokeWithLayer",
	0xc1cd5ea9: "initConnection",
	0xc4f9186b: "help.getConfig",
	0xd91a548:  "users.getUsers",
	0xb60f5918: "users.getFullUser",
	0xf93ccba3: "contacts.resolveUsername",
	0x280d096f: "messages.sendMessage",
	0x4423e6c5: "messages.getHistory",
	0x7abe77ec: "ping",
}

func ctorName(id uint32) string {
	if n, ok := ctorNames[id]; ok {
		return n
	}
	return "ctor"
}

// HooksAccessor returns a pointer to the live hooks struct so tests and
// advanced users can install callbacks before Connect. Mutations after
// Connect are visible to subsequent events (hooks are read per event).
func (c *Client) Hooks() *Hooks { return &c.cfg.Hooks }
