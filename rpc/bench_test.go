package rpc

import (
	"context"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func mtprotoEncryptForBench(k *crypto.AuthKey, payload []byte, rnd interface{ Read([]byte) (int, error) }) ([]byte, error) {
	return mtproto.EncryptMessageData(k, payload, rnd)
}

// ---- registry micro-benchmarks --------------------------------------------

func benchPending(b *testing.B, id int64) *pending {
	b.Helper()
	p := &pending{msgID: id, done: make(chan struct{})}
	p.state.Store(int32(stateRegistered))
	return p
}

func BenchmarkRegistryAdd(b *testing.B) {
	r, _ := newTestRegistry()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := benchPending(b, int64(i))
		r.add(p)
	}
}

func BenchmarkRegistryLookup(b *testing.B) {
	r, _ := newTestRegistry()
	for i := 0; i < 1024; i++ {
		p := benchPending(b, int64(i))
		p.state.Store(int32(stateSent))
		r.add(p)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r.lookup(int64(i % 1024))
	}
}

func BenchmarkRegistryComplete(b *testing.B) {
	r, _ := newTestRegistry()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := benchPending(b, int64(i))
		p.state.Store(int32(stateSent))
		r.add(p)
		b.StartTimer()
		r.finalizeByMsgID(int64(i), &gen.BoolTrue{}, nil)
	}
}

func BenchmarkRegistryConcurrentAddComplete(b *testing.B) {
	r, _ := newTestRegistry()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			p := benchPending(b, int64(i))
			p.state.Store(int32(stateSent))
			r.add(p)
			r.finalizeByMsgID(int64(i), &gen.BoolTrue{}, nil)
			i++
		}
	})
}

// ---- request framing --------------------------------------------------------

func BenchmarkRequestSerialize(b *testing.B) {
	req := &gen.PingRequest{PingID: 42}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := tl.NewWriter(64)
		if err := req.Encode(w); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnvelopeSeal(b *testing.B) {
	cfg := Config{AuthKey: testAuthKey()}
	cfg.fill()
	e := &Engine{cfg: cfg, ids: newIDAssigner(0), reg: newRegistry(&Metrics{})}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	e.replaySignal = make(chan struct{}, 1)
	body := mustMarshal(&gen.PingRequest{PingID: 42})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload := e.frameMessage(int64(i), 1, body, true)
		if _, err := sealEnvelope(&cfg, payload); err != nil {
			b.Fatal(err)
		}
	}
}

// sealEnvelope shares the sendCall encryption path.
func sealEnvelope(cfg *Config, payload []byte) ([]byte, error) {
	return mtprotoEncryptForBench(cfg.AuthKey, payload, cfg.Rand)
}
