package transport_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/transport"
)

// BenchmarkEndToEndLoopback measures send-side throughput through the full
// transport stack (outbound queue → abridged → obfuscated2 → WebSocket)
// against a fake peer that drains and discards.
func BenchmarkEndToEndLoopback1K(b *testing.B) {
	benchEndToEnd(b, 1024, 1)
}

func BenchmarkEndToEndLoopback1Kx16Producers(b *testing.B) {
	benchEndToEnd(b, 1024, 16)
}

func benchEndToEnd(b *testing.B, size int, producers int) {
	srv, err := wstest.New()
	if err != nil {
		b.Fatal(err)
	}
	defer srv.Close()

	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, nil)
		peer.Wait()
	}

	tr := transport.New(testConfig(srv.Addr))
	if err := tr.Connect(context.Background()); err != nil {
		b.Fatal(err)
	}
	payload := make([]byte, size)
	b.SetBytes(int64(size))
	b.ResetTimer()

	var wg sync.WaitGroup
	wg.Add(producers)
	for p := 0; p < producers; p++ {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			for i := 0; i < b.N/producers; i++ {
				if err := tr.Send(ctx, payload); err != nil {
					b.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	tr.Close()
}
