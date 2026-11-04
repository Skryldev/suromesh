package session_test

import (
	"crypto/rand"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/session"
)

func benchKey() *crypto.AuthKey {
	var b [256]byte
	_, _ = rand.Read(b[:])
	return crypto.NewAuthKey(b)
}

func BenchmarkStringSessionParse(b *testing.B) {
	ss := &session.StringSession{
		DCID: 2, ServerAddr: "im-server.splus.ir", Port: 443,
		AuthKeyValue: benchKey().Key, HasAuthKey: true,
	}
	encoded := ss.String()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := session.ParseStringSession(encoded); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStringSessionSerialize(b *testing.B) {
	ss := &session.StringSession{
		DCID: 2, ServerAddr: "im-server.splus.ir", Port: 443,
		AuthKeyValue: benchKey().Key, HasAuthKey: true,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ss.String()
	}
}

func BenchmarkFileSessionSave(b *testing.B) {
	dir := b.TempDir()
	path := dir + "/bench.bin"
	fs, err := session.OpenFile(path)
	if err != nil {
		b.Fatal(err)
	}
	_ = fs.SetDC(2, "im-server.splus.ir", 443)
	_ = fs.SetAuthKey(benchKey())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fs.Save(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFileSessionLoad(b *testing.B) {
	dir := b.TempDir()
	path := dir + "/bench.bin"
	fs, err := session.OpenFile(path)
	if err != nil {
		b.Fatal(err)
	}
	_ = fs.SetDC(2, "im-server.splus.ir", 443)
	_ = fs.SetAuthKey(benchKey())
	if err := fs.Save(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := session.OpenFile(path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExchangeFull(b *testing.B) {
	b.Skip("covered by tests/auth E2E timing; full DH exchange per-op ≈ RSA modexp + 3 round trips")
}
