package rpc

import (
	"context"
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// newFuzzEngine builds an engine with no transport: dispatch-path fuzzing
// never touches the network, only registry/state handling.
func newFuzzEngine() *Engine {
	cfg := Config{AuthKey: testAuthKey()}
	cfg.fill()
	e := &Engine{
		cfg:     cfg,
		ids:     newIDAssigner(0),
		updates: make(chan tl.Object, 4),
		done:    make(chan struct{}),
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.reg = newRegistry(&e.metrics)
	e.replaySignal = make(chan struct{}, 1)
	return e
}

// FuzzHandlePayload feeds arbitrary decrypted payloads (the post-envelope
// seam) through the dispatcher. Malformed RPC responses, hostile message
// IDs, nested containers, and unknown types must never panic or leak
// registry entries.
func FuzzHandlePayload(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 16))
	f.Add([]byte{
		0x01, 0x78, 0x29, 0xcb, // labeledPrice ctor
		0x01, 0x00, 0x00, 0x00, // msg_id
		0x01, 0x00, 0x00, 0x00, // seq
		0x04, 0x00, 0x00, 0x00, // len 4
	})
	// rpc_result with rpc_error inside
	errObj := mustMarshal(&tl.RpcError{ErrorCode: 420, ErrorMessage: "FLOOD_WAIT_3"})
	rr := mustMarshal(&tl.RpcResult{ReqMsgID: 7, Error: &tl.RpcError{ErrorCode: 400, ErrorMessage: "X"}})
	f.Add(rr)
	f.Add(errObj)
	// nested container seed
	nested := &tl.MessageContainer{Messages: []tl.TLMessage{
		{MsgID: 1, SeqNo: 1, Obj: &tl.RpcResult{ReqMsgID: 2, Body: mustMarshal(&gen.BoolTrue{})}},
	}}
	f.Add(mustMarshal(nested))

	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 1<<16 {
			return
		}
		e := newFuzzEngine()
		defer e.cancel()

		// Register one pending request that hostile data may or may not hit.
		p := &pending{msgID: 7, done: make(chan struct{})}
		p.state.Store(int32(stateRegistered))
		e.reg.add(p)

		acks := e.handlePayload(payload, nil)

		// Invariants: registry never holds more than the one seeded entry,
		// and done is closed iff the entry was removed.
		if e.reg.len() > 1 {
			t.Fatalf("registry grew under fuzz: %d", e.reg.len())
		}
		if got, ok := e.reg.lookup(7); ok && got.isDone() {
			t.Fatal("completed request still registered")
		}
		_ = acks
	})
}

// FuzzEnvelopeDecrypt feeds arbitrary transport packets through the inbound
// decrypt path (hostile envelopes must be counted errors, never panics).
func FuzzEnvelopeDecrypt(f *testing.F) {
	f.Add(make([]byte, 40))
	f.Add(make([]byte, 128))
	f.Fuzz(func(t *testing.T, pkt []byte) {
		if len(pkt) > 1<<16 {
			return
		}
		e := newFuzzEngine()
		defer e.cancel()
		if plain, err := mtprotoDecrypt(e.cfg.AuthKey, pkt); err == nil {
			e.handlePayload(plain, nil)
		} else {
			e.metrics.EnvelopeErrors.Add(1)
		}
	})
}
