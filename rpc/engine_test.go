package rpc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
	"github.com/Skryldev/suromesh/transport"
)

func testAuthKey() *crypto.AuthKey {
	var key [256]byte
	for i := range key {
		key[i] = byte(i * 7)
	}
	return crypto.NewAuthKey(key)
}

// ---- registry --------------------------------------------------------------

func newTestRegistry() (*registry, *Metrics) {
	m := &Metrics{}
	return newRegistry(m), m
}

func TestRegistryAddLookupComplete(t *testing.T) {
	r, m := newTestRegistry()
	p := &pending{msgID: 42, done: make(chan struct{})}
	p.state.Store(int32(stateRegistered))
	r.add(p)

	got, ok := r.lookup(42)
	if !ok || got != p {
		t.Fatal("lookup failed")
	}
	if r.len() != 1 {
		t.Fatalf("len %d", r.len())
	}

	// First finalize wins; a response after removal is classified as
	// unknown/late (reference: "response without parent request") — either
	// way it must never complete anything again.
	if _, won := r.finalizeByMsgID(42, &gen.BoolTrue{}, nil); !won {
		t.Fatal("first completion must win")
	}
	if _, won := r.finalizeByMsgID(42, nil, nil); won {
		t.Fatal("second completion must lose")
	}
	if m.UnknownResponses.Load() != 1 {
		t.Fatal("post-removal response not classified")
	}
	if r.len() != 0 {
		t.Fatalf("completed request leaked from registry: len=%d", r.len())
	}
	if m.RequestsCompleted.Load() != 1 {
		t.Fatal("completion not counted")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("done not closed")
	}
	if p.result == nil || p.err != nil {
		t.Fatal("outcome not stored")
	}
}

func TestRegistryFailAll(t *testing.T) {
	r, _ := newTestRegistry()
	const n = 1000
	for i := 0; i < n; i++ {
		p := &pending{msgID: int64(i), done: make(chan struct{})}
		p.state.Store(int32(stateRegistered))
		r.add(p)
	}
	if lost := r.failAll(ErrConnectionLost); lost != n {
		t.Fatalf("failAll finalized %d, want %d", lost, n)
	}
	if r.len() != 0 {
		t.Fatalf("registry leaked %d entries", r.len())
	}
	// Double failAll is a no-op.
	if lost := r.failAll(ErrConnectionLost); lost != 0 {
		t.Fatalf("second failAll finalized %d", lost)
	}
}

func TestRegistryFinalizeIsExactlyOnce(t *testing.T) {
	r, _ := newTestRegistry()
	p := &pending{msgID: 1, done: make(chan struct{})}
	p.state.Store(int32(stateRegistered))
	r.add(p)

	const racers = 64
	var wins atomic64
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			if _, won := r.finalizeByMsgID(1, nil, fmt.Errorf("racer %d", i)); won {
				wins.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("exactly one winner required, got %d", wins.Load())
	}
}

// atomic64 is a tiny atomic counter (keeps the test dependency-free).
type atomic64 struct {
	mu sync.Mutex
	v  int
}

func (a *atomic64) Add(n int) { a.mu.Lock(); a.v += n; a.mu.Unlock() }
func (a *atomic64) Load() int { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// ---- id assigner ------------------------------------------------------------

func TestIDAssignerConcurrentUniqueness(t *testing.T) {
	for _, goroutines := range []int{1, 10, 100, 1000, 10000} {
		t.Run(fmt.Sprintf("g=%d", goroutines), func(t *testing.T) {
			a := newIDAssigner(0)
			const perG = 100
			ids := make([]int64, goroutines*perG)
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				go func(g int) {
					defer wg.Done()
					for i := 0; i < perG; i++ {
						id, _ := a.assign(true)
						ids[g*perG+i] = id
					}
				}(g)
			}
			wg.Wait()

			seen := make(map[int64]struct{}, len(ids))
			var maxID int64
			for _, id := range ids {
				if id%4 != 0 {
					t.Fatalf("msg_id %d not congruent 0 mod 4", id)
				}
				if _, dup := seen[id]; dup {
					t.Fatalf("duplicate msg_id %d at %d goroutines", id, goroutines)
				}
				seen[id] = struct{}{}
				if id > maxID {
					maxID = id
				}
			}
			_ = maxID
		})
	}
}

func TestSeqNoSemantics(t *testing.T) {
	a := newIDAssigner(0)
	// Content-related messages: odd numbers, counter increments.
	for want := int32(1); want <= 9; want += 2 {
		_, seq := a.assign(true)
		if seq != want {
			t.Fatalf("content seq %d, want %d", seq, want)
		}
	}
	// Control messages: even, counter does NOT advance — consecutive
	// control messages share the same even seq_no (reference
	// _get_seq_no(false) returns seq*2 without incrementing).
	for i := 0; i < 4; i++ {
		if _, seq := a.assign(false); seq != 10 {
			t.Fatalf("control seq %d, want 10", seq)
		}
	}
	// Interleaving keeps the invariant: the next content message continues
	// from the untouched counter.
	_, seq := a.assign(true)
	if seq != 11 {
		t.Fatalf("interleaved content seq %d, want 11", seq)
	}
	// bad-msg 32 recovery: +64 jump (counter was 6 after the interleave).
	a.advanceSeq(64)
	_, seq = a.assign(true)
	if seq != 2*70+1 {
		t.Fatalf("post-advance seq %d, want 141", seq)
	}
	// bad-msg 33 recovery: -16 adjustment (counter 71-16=55; Next(true)
	// had advanced it to 71 after the +64 jump).
	a.advanceSeq(-16)
	_, seq = a.assign(true)
	if seq != 2*55+1 {
		t.Fatalf("post-decrease seq %d, want 111", seq)
	}
}

func TestIDAssignerTimeOffsetCorrection(t *testing.T) {
	a := newIDAssigner(0)
	// Server msg_id implies a clock 100s ahead of ours.
	serverMsgID := (unixNow() + 100) << 32
	a.setTimeOffset(serverMsgID, unixNow())
	id, _ := a.assign(true)
	seconds := id >> 32
	if diff := seconds - unixNow(); diff < 99 || diff > 101 {
		t.Fatalf("offset correction produced %d (want ~+100)", diff)
	}
	// Monotonicity holds across the offset change.
	id2, _ := a.assign(true)
	if id2 <= id {
		t.Fatal("non-monotonic across offset correction")
	}
}

func unixNow() int64 { return time.Now().Unix() }

// ---- RPC error parsing -------------------------------------------------------

func TestParseRPCError(t *testing.T) {
	cases := []struct {
		code     int32
		message  string
		wantType string
		wantArg  int
	}{
		{420, "FLOOD_WAIT_17", "FLOOD_WAIT", 17},
		{420, "FLOOD_PREMIUM_WAIT_3600", "FLOOD_PREMIUM_WAIT", 3600},
		{400, "PHONE_NUMBER_INVALID", "PHONE_NUMBER_INVALID", -1},
		{500, "NO_WORKERS_RUNNING", "NO_WORKERS_RUNNING", -1},
		{400, "TIMEOUT", "TIMEOUT", -1},
		{420, "SLOWMODE_WAIT_5", "SLOWMODE_WAIT", 5},
	}
	for _, c := range cases {
		e := parseRPCError(c.code, c.message)
		if e.Code != c.code || e.Type != c.wantType || e.Argument != c.wantArg {
			t.Fatalf("parse(%d, %q) = %+v", c.code, c.message, e)
		}
	}
	flood := parseRPCError(420, "FLOOD_WAIT_17")
	if d, ok := flood.RetryAfter(); !ok || d != 17*time.Second {
		t.Fatalf("RetryAfter = %v %v", d, ok)
	}
	if !flood.IsFloodWait() {
		t.Fatal("IsFloodWait false")
	}
	plain := parseRPCError(400, "PEER_ID_INVALID")
	if _, ok := plain.RetryAfter(); ok {
		t.Fatal("non-flood error reported RetryAfter")
	}
}

// ---- dispatch routing (no transport) -----------------------------------------

func newDispatchTestEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := Config{AuthKey: testAuthKey(), UpdatesQueue: 8}
	cfg.fill()
	e := &Engine{
		cfg:     cfg,
		ids:     newIDAssigner(0),
		updates: make(chan tl.Object, 8),
		done:    make(chan struct{}),
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.reg = newRegistry(&e.metrics)
	return e
}

func TestDispatchRoutesUpdateToUpdatesChannel(t *testing.T) {
	e := newDispatchTestEngine(t)
	defer e.cancel()

	// An update must NOT complete a pending request even if its msg_id
	// collides with a registered one.
	p := &pending{msgID: 777, done: make(chan struct{})}
	p.state.Store(int32(stateRegistered))
	e.reg.add(p)

	upd := &gen.UpdateNewMessage{PTS: 1, PTSCount: 1, Message: &gen.MessageEmpty{ID: 1}}
	if err := e.handleMessage(999, 2, upd, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-e.updates:
		if got != tl.Object(upd) {
			t.Fatal("wrong update delivered")
		}
	default:
		t.Fatal("update not routed to the updates channel")
	}
	if p.isDone() {
		t.Fatal("update satisfied a pending request")
	}
}

func TestDispatchUpdatesChannelOverflowDrops(t *testing.T) {
	e := newDispatchTestEngine(t)
	defer e.cancel()
	for i := 0; i < 100; i++ {
		_ = e.handleMessage(int64(i), 0, &gen.BoolTrue{}, 0) // not an update
		_ = e.handleMessage(int64(i), 0, &gen.UpdateConfig{}, 0)
	}
	if e.metrics.UpdatesDropped.Load() == 0 {
		t.Fatal("overflow drops not counted")
	}
}

func TestDispatchMalformedBodyIsContained(t *testing.T) {
	e := newDispatchTestEngine(t)
	defer e.cancel()
	before := e.reg.len()
	// Malformed top-level bodies fail in tl.Unmarshal before handleMessage;
	// exercise the contained path with truncated gzip payloads instead.
	for i := 0; i < 100; i++ {
		_ = e.handleMessage(int64(i), 0, &tl.GzipPacked{Data: []byte{0xDE, 0xAD, 0xBE}}, 0)
	}
	if e.reg.len() != before {
		t.Fatal("registry changed on malformed input")
	}
	if e.metrics.ProtocolErrors.Load() == 0 {
		t.Fatal("protocol errors not counted")
	}
}

func TestDispatchContainerCorrelatesIndependently(t *testing.T) {
	e := newDispatchTestEngine(t)
	defer e.cancel()

	p1 := &pending{msgID: 11, done: make(chan struct{})}
	p2 := &pending{msgID: 22, done: make(chan struct{})}
	for _, p := range []*pending{p1, p2} {
		p.state.Store(int32(stateRegistered))
		e.reg.add(p)
	}

	container := &tl.MessageContainer{Messages: []tl.TLMessage{
		{MsgID: 11, SeqNo: 1, Obj: &tl.RpcResult{ReqMsgID: 11, Body: mustMarshal(&gen.BoolTrue{})}},
		{MsgID: 22, SeqNo: 3, Obj: &tl.RpcResult{ReqMsgID: 22, Error: &tl.RpcError{ErrorCode: 420, ErrorMessage: "FLOOD_WAIT_3"}}},
		{MsgID: 33, SeqNo: 5, Obj: &gen.Pong{MsgID: 33, PingID: 9}}, // unrelated service msg
	}}
	if err := e.handleMessage(0, 0, container, 0); err != nil {
		t.Fatal(err)
	}

	<-p1.done
	if p1.err != nil {
		t.Fatalf("p1 failed: %v", p1.err)
	}
	if _, ok := p1.result.(*gen.BoolTrue); !ok {
		t.Fatalf("p1 result %T", p1.result)
	}
	<-p2.done
	var rpcErr *RPCError
	if !errors.As(p2.err, &rpcErr) || rpcErr.Code != 420 || rpcErr.Argument != 3 {
		t.Fatalf("p2 error %v", p2.err)
	}
	// The sibling failure must not have prevented service handling.
	if e.reg.len() != 0 {
		t.Fatalf("registry leak: %d", e.reg.len())
	}
}

func TestDispatchNestedContainerBounded(t *testing.T) {
	e := newDispatchTestEngine(t)
	defer e.cancel()
	// Build nesting beyond maxNesting: must error, not recurse forever.
	var deep tl.Object = &gen.BoolTrue{}
	for i := 0; i < maxNesting+3; i++ {
		deep = &tl.MessageContainer{Messages: []tl.TLMessage{{MsgID: int64(i), SeqNo: 1, Obj: deep}}}
	}
	// The over-deep branch is contained and counted, never propagated as a
	// crash and never allowed to recurse further.
	_ = e.handleMessage(1, 1, deep, 0)
	if e.metrics.ProtocolErrors.Load() == 0 {
		t.Fatal("nesting overflow not contained/counted")
	}
}

func TestDispatchGzipPackedRPCResult(t *testing.T) {
	e := newDispatchTestEngine(t)
	defer e.cancel()
	p := &pending{msgID: 5, done: make(chan struct{})}
	p.state.Store(int32(stateRegistered))
	e.reg.add(p)

	inner := &tl.RpcResult{ReqMsgID: 5, Body: mustMarshal(&gen.BoolTrue{})}
	gz := &tl.GzipPacked{Data: mustMarshal(inner)}
	if err := e.handleMessage(6, 2, gz, 0); err != nil {
		t.Fatal(err)
	}
	<-p.done
	if p.err != nil {
		t.Fatalf("gzip result failed: %v", p.err)
	}
}

func mustMarshal(o tl.Object) []byte {
	b, err := tl.Marshal(o)
	if err != nil {
		panic(err)
	}
	return b
}

func TestHandleBadServerSaltResends(t *testing.T) {
	// Attach a transport whose sends fail (closed) so the resend attempt
	// is exercised end to end and fails the pending deterministically.
	tr := transport.New(transport.Config{
		Host:          "127.0.0.1:1",
		PingInterval:  -1,
		MaxReconnects: 1,
		BackoffBase:   time.Millisecond,
		BackoffMax:    time.Millisecond,
	})
	_ = tr.Connect(context.Background()) // exhausts fast; transport closed
	e := newDispatchTestEngine(t)
	defer e.cancel()
	e.tr = tr
	e.cfg.ResendOnBadSalt = true

	p := &pending{msgID: 42, done: make(chan struct{})}
	p.state.Store(int32(stateRegistered))
	e.reg.add(p)

	e.handleBadServerSalt(&gen.BadServerSalt{BadMsgID: 42, NewServerSalt: 0x1234})
	if e.salt.Load() != 0x1234 {
		t.Fatal("salt not adopted")
	}
	if e.metrics.Resends.Load() != 1 {
		t.Fatal("resend not counted")
	}
	<-p.done
	if p.err == nil {
		t.Fatal("resend failure did not fail the pending request")
	}
	if e.reg.len() != 0 {
		t.Fatalf("registry leak after failed resend: %d", e.reg.len())
	}
}
