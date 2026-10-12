package client_test

// Phase 9 benchmarks: authentication is not a hot path — these pin the
// cost of the pure operations (state reads, conversions, classification)
// and the session persistence round trip. No cryptography is benchmarked
// (SRP cost is dominated by the protocol-mandated 100000-iteration KDF;
// optimizing it is out of scope and inadvisable).

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/session"
)

func BenchmarkAuthStateRead(b *testing.B) {
	c, _, _ := newAuthClient(b)
	defer c.Close()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = c.AuthState()
	}
}

func BenchmarkParsePhone(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := client.ParsePhone("+34 (123) 123-1234"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAuthErrorClassification(b *testing.B) {
	err := client.ErrPasswordRequired
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !isAuthErrorForBench(err) {
			b.Fatal("not classified")
		}
	}
}

func isAuthErrorForBench(err error) bool {
	return err != nil
}

func BenchmarkAuthSendCodeE2E(b *testing.B) {
	c, _, _ := newAuthClient(b)
	defer c.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.SendCode(context.Background(), testPhone); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAuthSignInE2E(b *testing.B) {
	c, _, _ := newAuthClient(b)
	defer c.Close()
	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// The fake accepts the code repeatedly; each signIn is a full
		// transport round trip plus the onAuthorization path.
		if _, err := c.SignIn(context.Background(), testPhone, "54321"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSessionSaveLoad(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "bench.session")
	fs, err := session.OpenFile(path)
	if err != nil {
		b.Fatal(err)
	}
	key := testAuthKeyOf(9)
	if err := fs.SetAuthKey(key); err != nil {
		b.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fs.Save(); err != nil {
			b.Fatal(err)
		}
		rl, err := session.OpenFile(path)
		if err != nil {
			b.Fatal(err)
		}
		if rl.AuthState() != session.StateAuthorized {
			b.Fatal("state lost")
		}
		rl.Close()
	}
}
