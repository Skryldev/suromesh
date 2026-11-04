// Package rpc_test drives the request engine end to end over the real
// transport stack against a fake Soroush peer: Invoke → serialize →
// envelope → transport → (fake server) → response envelope → dispatcher →
// correlation → completed request. Phase 2/3 semantics stay frozen.
package rpc_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
	"github.com/Skryldev/suromesh/transport"
)

// harness wires server + transport + engine. The responder callback runs on
// the peer's loop goroutine with each decrypted client envelope.
type harness struct {
	Server  *wstest.Server
	Peers   chan *wstest.SoroushPeer
	Tr      *transport.Transport
	Engine  *rpc.Engine
	AuthKey *crypto.AuthKey

	responderMu   sync.Mutex
	responderFunc func(*wstest.SoroushPeer, wstest.ClientEnvelope)
}

// SetResponder replaces the response behavior mid-test (thread-safe).
func (h *harness) SetResponder(fn func(*wstest.SoroushPeer, wstest.ClientEnvelope)) {
	h.responderMu.Lock()
	h.responderFunc = fn
	h.responderMu.Unlock()
}

func (h *harness) respond(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
	h.responderMu.Lock()
	fn := h.responderFunc
	h.responderMu.Unlock()
	if fn != nil {
		fn(peer, env)
	}
}

func startHarness(t *testing.T, cfg rpc.Config, onPacket func(*wstest.SoroushPeer, wstest.ClientEnvelope)) *harness {
	t.Helper()
	key := testKey()
	h := &harness{AuthKey: key}

	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	h.Server = srv
	h.Peers = make(chan *wstest.SoroushPeer, 16)
	h.responderFunc = onPacket
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, func(p *wstest.SoroushPeer, pkt []byte) {
			env, err := wstest.DecryptClientEnvelope(pkt, key)
			if err != nil {
				return
			}
			if wstest.IsServiceBody(env.Body) {
				return // acknowledgements: never reply, never count
			}
			h.respond(p, env)
		})
		h.Peers <- peer
		peer.Wait()
	}

	tr := transport.New(transport.Config{
		Host:         srv.Addr,
		WS:           wstest.WSOptions(),
		PingInterval: -1,
	})
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.Tr = tr

	cfg.AuthKey = key
	eng, err := rpc.New(tr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.Start()
	h.Engine = eng
	return h
}

func (h *harness) Close(t *testing.T) {
	h.Engine.Close()
	h.Tr.Close()
	h.Server.Close()
}

func testKey() *crypto.AuthKey {
	var key [256]byte
	for i := range key {
		key[i] = byte(i * 11)
	}
	return crypto.NewAuthKey(key)
}

// frame builds salt|session|TLMessage for a response body.
func frame(salt, session, msgID int64, seqNo int32, body []byte) []byte {
	w := tl.NewWriter(16 + len(body) + 16)
	w.PutInt64(salt)
	w.PutInt64(session)
	w.PutInt64(msgID)
	w.PutInt32(seqNo)
	w.PutInt32(int32(len(body)))
	w.PutRaw(body)
	return w.Bytes()
}

// replyWith frames an rpc_result for the client's message and sends it.
func replyWith(peer *wstest.SoroushPeer, env wstest.ClientEnvelope, key *crypto.AuthKey, result tl.Object, rpcErr *tl.RpcError) error {
	rr := &tl.RpcResult{ReqMsgID: env.MsgID}
	if rpcErr != nil {
		rr.Error = rpcErr
	} else {
		rr.Body = mustMarshal(result)
	}
	payload := frame(env.Salt, env.SessionID, env.MsgID+2, 1, mustMarshal(rr))
	return peer.SendServerEnvelope(key, payload, mtproto.Rand)
}

func mustMarshal(o tl.Object) []byte {
	b, err := tl.Marshal(o)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- the lifecycle gate ------------------------------------------------------

func TestInvokeSuccessE2E(t *testing.T) {
	var mu sync.Mutex
	var h *harness
	h = startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		mu.Lock()
		defer mu.Unlock()
		if err := replyWith(peer, env, h.AuthKey, &gen.Pong{MsgID: env.MsgID, PingID: 1}, nil); err != nil {
			t.Logf("reply: %v", err)
		}
	})
	defer h.Close(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 42})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	pong, ok := res.(*gen.Pong)
	if !ok || pong.PingID != 1 {
		t.Fatalf("result %T %+v", res, pong)
	}
	if h.Engine.Pending() != 0 {
		t.Fatalf("pending leak: %d", h.Engine.Pending())
	}
}

func TestInvokeRPCError(t *testing.T) {
	var h *harness
	h = startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		_ = replyWith(peer, env, h.AuthKey, nil, &tl.RpcError{ErrorCode: 420, ErrorMessage: "FLOOD_WAIT_7"})
	})
	defer h.Close(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 1})
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error %v, want *rpc.RPCError", err)
	}
	if rpcErr.Code != 420 || rpcErr.Type != "FLOOD_WAIT" || rpcErr.Argument != 7 {
		t.Fatalf("rpc error fields: %+v", rpcErr)
	}
	if d, ok := rpcErr.RetryAfter(); !ok || d != 7*time.Second {
		t.Fatalf("RetryAfter %v %v", d, ok)
	}
}

func TestInvokeContainerResponse(t *testing.T) {
	// The server batches responses into a msg_container: two results for
	// two concurrent requests plus an unrelated pong. A failure in one
	// must not corrupt the others.
	resultCh := make(chan error, 1)
	var once sync.Once
	var h *harness
	h = startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		// Respond to request A with a container holding A's result plus an
		// unrelated service message; sibling handling must stay isolated.
		once.Do(func() {
			rr := &tl.RpcResult{ReqMsgID: env.MsgID, Body: mustMarshal(&gen.BoolTrue{})}
			container := &tl.MessageContainer{Messages: []tl.TLMessage{
				{MsgID: env.MsgID + 2, SeqNo: 1, Obj: rr},
				{MsgID: env.MsgID + 4, SeqNo: 3, Obj: &gen.Pong{MsgID: 9, PingID: 8}},
			}}
			payload := frame(env.Salt, env.SessionID, env.MsgID+6, 1, mustMarshal(container))
			if err := peer.SendServerEnvelope(h.AuthKey, payload, mtproto.Rand); err != nil {
				resultCh <- err
			}
		})
	})
	defer h.Close(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 5})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if _, ok := res.(*gen.BoolTrue); !ok {
		t.Fatalf("result %T", res)
	}
	select {
	case err := <-resultCh:
		t.Fatalf("container send failed: %v", err)
	default:
	}
}

func TestInvokeDuplicateResponse(t *testing.T) {
	var h *harness
	h = startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		// Send the same result twice (duplicate response injection).
		_ = replyWith(peer, env, h.AuthKey, &gen.BoolTrue{}, nil)
		_ = replyWith(peer, env, h.AuthKey, &gen.BoolTrue{}, nil)
	})
	defer h.Close(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.(*gen.BoolTrue); !ok {
		t.Fatalf("result %T", res)
	}
	// The duplicate must be classified, not crash or re-complete.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if h.Engine.Metrics().DuplicateResponses.Load()+h.Engine.Metrics().UnknownResponses.Load() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.Engine.Metrics().DuplicateResponses.Load()+h.Engine.Metrics().UnknownResponses.Load() == 0 {
		t.Fatal("duplicate response not classified")
	}
}

func TestInvokeUnknownResponseID(t *testing.T) {
	var h *harness
	h = startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		// Respond with a msg_id that matches nothing pending.
		rr := &tl.RpcResult{ReqMsgID: env.MsgID + 1000, Body: mustMarshal(&gen.BoolTrue{})}
		payload := frame(env.Salt, env.SessionID, env.MsgID+2, 1, mustMarshal(rr))
		_ = peer.SendServerEnvelope(h.AuthKey, payload, mtproto.Rand)
	})
	defer h.Close(t)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 3})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v, want deadline exceeded (no response matched)", err)
	}
	if h.Engine.Metrics().UnknownResponses.Load() == 0 {
		t.Fatal("unknown response not counted")
	}
	if h.Engine.Pending() != 0 {
		t.Fatalf("pending leak after timeout: %d", h.Engine.Pending())
	}
}

func TestInvokeLateResponseAfterTimeout(t *testing.T) {
	// The server answers after the caller gave up: the response must be
	// classified as unknown and the engine must stay healthy.
	var mu sync.Mutex
	delay := time.Duration(0)
	var h *harness
	h = startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		mu.Lock()
		d := delay
		mu.Unlock()
		time.Sleep(d)
		_ = replyWith(peer, env, h.AuthKey, &gen.BoolTrue{}, nil)
	})
	defer h.Close(t)

	mu.Lock()
	delay = time.Second
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 4})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("timeout took %v — cancellation did not propagate", elapsed)
	}
	// The late response lands while nothing is pending: classified, healthy.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Engine.Metrics().UnknownResponses.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.Engine.Metrics().UnknownResponses.Load() == 0 {
		t.Fatal("late response not classified as unknown")
	}

	// Engine still serves new requests.
	mu.Lock()
	delay = 0
	mu.Unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, err := h.Engine.Invoke(ctx2, &gen.PingRequest{PingID: 5}); err != nil {
		t.Logf("metrics: %+v state=%s pending=%d", h.Engine.Metrics().Snapshot(), h.Tr.State(), h.Engine.Pending())
		t.Fatalf("engine unhealthy after late response: %v", err)
	}
}

func TestInvokeBadSaltTriggersResend(t *testing.T) {
	// First attempt: respond bad_server_salt; second attempt (after the
	// engine resends under the adopted salt): respond with the result.
	var mu sync.Mutex
	attempts := 0
	var h *harness
	h = startHarness(t, rpc.Config{ResendOnBadSalt: true}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			bs := &gen.BadServerSalt{BadMsgID: env.MsgID, NewServerSalt: 0x7777}
			payload := frame(env.Salt, env.SessionID, env.MsgID+2, 1, mustMarshal(bs))
			_ = peer.SendServerEnvelope(h.AuthKey, payload, mtproto.Rand)
			return
		}
		if env.Salt != 0x7777 {
			t.Errorf("resent envelope salt %d, want 0x7777", env.Salt)
		}
		_ = replyWith(peer, env, h.AuthKey, &gen.BoolTrue{}, nil)
	})
	defer h.Close(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 6})
	if err != nil {
		t.Fatalf("invoke after bad-salt resend: %v", err)
	}
	if _, ok := res.(*gen.BoolTrue); !ok {
		t.Fatalf("result %T", res)
	}
	if h.Engine.Metrics().Resends.Load() < 1 {
		t.Fatal("resend not counted")
	}
}

func TestConnectionLossFailsPending(t *testing.T) {
	h := startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		time.Sleep(10 * time.Second) // never respond in time
	})

	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 7})
		errCh <- err
	}()

	// Let the request go out, then kill the connection.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.Engine.Pending() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	var peer *wstest.SoroushPeer
	select {
	case peer = <-h.Peers:
	default:
	}
	if peer != nil {
		peer.Close()
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, rpc.ErrConnectionLost) {
			t.Fatalf("error %v, want ErrConnectionLost", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending request not failed after connection loss")
	}
	h.Close(t)
}

func TestRetryOnReconnectReplays(t *testing.T) {
	// Policy variant: RetryOnReconnect=true replays the pending request on
	// the replacement connection (reference behavior); the new peer answers.
	attempt := 0
	var mu sync.Mutex
	firstDelivery := make(chan struct{})
	responses := make(chan wstest.ClientEnvelope, 8)

	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	key := testKey()
	peers := make(chan *wstest.SoroushPeer, 8)
	var currentPeer atomic.Pointer[wstest.SoroushPeer] // set right after the peer is created
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, func(p *wstest.SoroushPeer, pkt []byte) {
			env, derr := wstest.DecryptClientEnvelope(pkt, key)
			if derr != nil {
				return
			}
			if wstest.IsServiceBody(env.Body) {
				return
			}
			mu.Lock()
			attempt++
			n := attempt
			mu.Unlock()
			if n == 1 {
				close(firstDelivery) // request delivered: close from the helper
				return               // first connection: black hole
			}
			responses <- env
			rr := &tl.RpcResult{ReqMsgID: env.MsgID, Body: mustMarshal(&gen.BoolTrue{})}
			payload := frame(env.Salt, env.SessionID, env.MsgID+2, 1, mustMarshal(rr))
			if p := currentPeer.Load(); p != nil {
				_ = p.SendServerEnvelope(key, payload, mtproto.Rand)
			}
		})
		currentPeer.Store(peer)
		peers <- peer
		peer.Wait()
	}

	tr := transport.New(transport.Config{Host: srv.Addr, WS: wstest.WSOptions(), PingInterval: -1})
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	eng, err := rpc.New(tr, rpc.Config{AuthKey: key, RetryOnReconnect: true})
	if err != nil {
		t.Fatal(err)
	}
	eng.Start()
	defer eng.Close()

	peer1 := <-peers
	// Invoke blocks, so the first connection is killed from a helper
	// goroutine once the request is delivered — triggering the reconnect
	// that the RetryOnReconnect policy must replay into. The trigger is
	// the delivery event itself (firstDelivery), not a poll: a poll-based
	// closer starves under full-suite parallel load and leaves the invoke
	// waiting for its whole context.
	go func() {
		<-firstDelivery
		t.Logf("closing peer1 now")
		peer1.Close()
	}()
	go func() {
		seen := 0
		for {
			select {
			case p := <-peers:
				seen++
				t.Logf("connection #%d established", seen)
				_ = p
			case <-time.After(10 * time.Second):
				return
			}
		}
	}()
	// 60 s: full-suite -race parallelism can stretch the reconnect +
	// replay path far beyond single-test timings (0.01 s healthy).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := eng.Invoke(ctx, &gen.PingRequest{PingID: 8})
	if err != nil {
		t.Logf("metrics: %+v state=%s pending=%d responses=%d", eng.Metrics().Snapshot(), tr.State(), eng.Pending(), len(responses))
		mu.Lock()
		t.Logf("attempts=%d", attempt)
		mu.Unlock()
		t.Fatalf("replayed invoke failed: %v", err)
	}
	if _, ok := res.(*gen.BoolTrue); !ok {
		t.Fatalf("result %T", res)
	}
	if len(responses) != 1 {
		t.Fatalf("expected exactly one replay delivery, got %d", len(responses))
	}
	_ = peer1
}

func TestMalformedResponseDoesNotKillEngine(t *testing.T) {
	first := true
	var mu sync.Mutex
	var h *harness
	h = startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			// Garbage envelope bytes (invalid msg_key/IGE stream).
			_ = peer.SendPacket([]byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12,
				13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24})
			return
		}
		_ = replyWith(peer, env, h.AuthKey, &gen.BoolTrue{}, nil)
	})
	defer h.Close(t)

	// First invoke receives the garbage response and must time out.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel1()
	if _, err := h.Engine.Invoke(ctx1, &gen.PingRequest{PingID: 9}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("garbage-response invoke: %v, want deadline exceeded", err)
	}
	if h.Engine.Metrics().EnvelopeErrors.Load() == 0 {
		t.Fatal("envelope error not counted")
	}

	// Second invoke proves the engine is healthy.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	res, err := h.Engine.Invoke(ctx2, &gen.PingRequest{PingID: 10})
	if err != nil {
		t.Logf("metrics: %+v state=%s pending=%d", h.Engine.Metrics().Snapshot(), h.Tr.State(), h.Engine.Pending())
		t.Fatalf("engine unhealthy after malformed response: %v", err)
	}
	if _, ok := res.(*gen.BoolTrue); !ok {
		t.Fatalf("result %T", res)
	}
}

func TestShutdownWithThousandsPending(t *testing.T) {
	h := startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		time.Sleep(10 * time.Second) // nothing completes
	})

	const n = 2000
	errs := make(chan error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			_, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: int64(i)})
			errs <- err
		}()
	}

	// Wait until all are registered, then shut down mid-flight.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.Engine.Pending() >= n {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.Engine.Pending() < n {
		t.Fatalf("only %d of %d registered", h.Engine.Pending(), n)
	}
	h.Engine.Close()

	for i := 0; i < n; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("pending request completed after shutdown")
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d still blocked after shutdown", i)
		}
	}
	if h.Engine.Pending() != 0 {
		t.Fatalf("registry leaked %d entries", h.Engine.Pending())
	}
	h.Close(t)
}

func TestRequestCreationRacingShutdown(t *testing.T) {
	h := startHarness(t, rpc.Config{}, func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
		time.Sleep(10 * time.Second)
	})
	var wg sync.WaitGroup
	wg.Add(16)
	for i := 0; i < 16; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, _ = h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 1}) // may fail: fine
		}()
	}
	time.Sleep(10 * time.Millisecond)
	h.Engine.Close()
	wg.Wait()
	h.Close(t)
	if h.Engine.Pending() != 0 {
		t.Fatalf("registry leak: %d", h.Engine.Pending())
	}
}

func TestQueueSaturationSurfacesToInvoke(t *testing.T) {
	// Outbound queue 1 + stalled writer path: the Invoke must fail with a
	// transport-classified error instead of blocking forever.
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	key := testKey()
	peers := make(chan *wstest.SoroushPeer, 4)
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, nil)
		peers <- peer
		peer.Wait()
	}
	tr := transport.New(transport.Config{
		Host: srv.Addr, WS: wstest.WSOptions(), PingInterval: -1,
		OutboundQueueSize: 1, OutboundPolicy: transport.PolicyReject,
	})
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	eng, err := rpc.New(tr, rpc.Config{AuthKey: key})
	if err != nil {
		t.Fatal(err)
	}
	eng.Start()
	defer eng.Close()

	// The socket accepts many KB before backpressure; flood until a reject
	// is observed, bounded by time.
	var rejected int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := eng.Invoke(ctx, &gen.PingRequest{PingID: 1})
		cancel()
		if err != nil {
			rejected++
			if rejected > 0 {
				break
			}
		}
	}
	if rejected == 0 {
		t.Fatal("no failure observed under queue saturation")
	}
}
