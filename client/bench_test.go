package client_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func BenchmarkInvokePing(b *testing.B) {
	srv := newBenchServer(b)
	c := newReadyClientB(b, srv)
	defer c.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConcurrentInvoke(b *testing.B) {
	srv := newBenchServer(b)
	c := newReadyClientB(b, srv)
	defer c.Close()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)}); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func BenchmarkEntityConversion(b *testing.B) {
	c := newEntityHolder(b)
	u := &gen.User{ID: 42, AccessHash: i64p(7), Username: strp("x"), FirstName: strp("y"), UserType: &gen.UserTypeNormal{}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.SeedRaw(u)
	}
}

func BenchmarkStringSessionPhase6(b *testing.B) {
	b.Skip("covered in session package benchmarks")
}

func newBenchServer(b *testing.B) *wstest.Server {
	srv, err := wstestNew()
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { srv.Close() })
	return srv
}

func newReadyClientB(b *testing.B, srv *wstest.Server) *client.Client {
	c, key := newTestClient(b, srv.Addr)
	wireFakeAppServer(b, srv, key)
	if err := c.Connect(context.Background()); err != nil {
		b.Fatal(err)
	}
	return c
}

func newEntityHolder(b *testing.B) *client.Client {
	c, err := client.New(client.Config{Session: session.NewWithKey(testKey(9))})
	if err != nil {
		b.Fatal(err)
	}
	return c
}

func BenchmarkEntityCacheGet(b *testing.B) {
	srv := newBenchServer(b)
	c := newReadyClientB(b, srv)
	defer c.Close()
	c.SeedEntities([]*client.User{{ID: 1, AccessHash: 42}}, nil, nil)
	p := client.PeerID{Kind: client.PeerUser, ID: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InputPeerFor(context.Background(), p); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEntityCachePut(b *testing.B) {
	srv := newBenchServer(b)
	c := newReadyClientB(b, srv)
	defer c.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.SeedRaw(&gen.User{ID: int64(i), AccessHash: i64p(int64(i)), UserType: &gen.UserTypeNormal{}})
	}
}

func BenchmarkUpdateDispatch(b *testing.B) {
	srv := newBenchServer(b)
	c, _ := newTestClient(b, srv.Addr)
	fake := wireFakeAppServer(b, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		b.Fatal(err)
	}
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.UpdateShort{Update: &gen.UpdateConfig{}, Date: 1}, nil
	})

	var received int64 = -1
	done := make(chan struct{})
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if atomic.AddInt64(&received, 1) == int64(b.N)-1 {
			close(done)
		}
	})
	defer sub.Unsubscribe()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)}); err != nil {
			b.Fatal(err)
		}
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		b.Fatal("updates not drained")
	}
}
