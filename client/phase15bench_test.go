package client_test

// Phase 15 benchmarks: reconnect overhead, Close latency, pending
// throughput, persistence throughput.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl/gen"
)

func BenchmarkConnectCloseCycle(b *testing.B) {
	base := filepath.Join(b.TempDir(), "cc.session")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		srv := newWSTestServerForAuth(b)
		fs, err := session.OpenFile(base)
		if err != nil {
			b.Fatal(err)
		}
		c, _ := newAuthClientWithSession(b, srv, fs)
		_, _ = c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)})
		_ = c.Close()
		srv.Close()
	}
}

func BenchmarkPendingThroughput(b *testing.B) {
	c, _, _ := newDialogsClient(b)
	defer c.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionSave(b *testing.B) {
	fs, err := session.OpenFile(filepath.Join(b.TempDir(), "save.session"))
	if err != nil {
		b.Fatal(err)
	}
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i + 1)
	}
	if err := fs.SetAuthKey(crypto.NewAuthKey(kb)); err != nil {
		b.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		b.Fatal(err)
	}
	if err := fs.SetUpdateState(session.UpdateStateRecord{Pts: 100, Date: 100}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fs.Save(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCloseLatency(b *testing.B) {
	servers := make([]*wsServerRef, 0, b.N)
	defer func() {
		for _, s := range servers {
			s.srv.Close()
		}
	}()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		srv := newWSTestServerForAuth(b)
		c, _, _ := newDialogsClientCfg(b, nil) //nolint
		start := time.Now()
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
		elapsed := time.Since(start)
		if elapsed > 2*time.Second {
			b.Fatalf("Close latency %v exceeds 2s", elapsed)
		}
		servers = append(servers, &wsServerRef{srv})
	}
}

type wsServerRef struct{ srv *wstest.Server }
