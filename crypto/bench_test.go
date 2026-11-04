package crypto_test

import (
	"bytes"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
)

func BenchmarkIGE1K(b *testing.B) {
	key := bytes.Repeat([]byte{7}, 32)
	iv := bytes.Repeat([]byte{9}, 32)
	plain := bytes.Repeat([]byte{1}, 1024)
	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := crypto.EncryptIGE(plain, key, iv); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkIGE16K(b *testing.B) {
	key := bytes.Repeat([]byte{7}, 32)
	iv := bytes.Repeat([]byte{9}, 32)
	plain := bytes.Repeat([]byte{1}, 16*1024)
	b.SetBytes(int64(len(plain)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := crypto.EncryptIGE(plain, key, iv); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkObfuscatedStream(b *testing.B) {
	random := make([]byte, 64)
	for i := range random {
		random[i] = byte(i * 5)
	}
	_, stream, err := crypto.InitObfuscated(bytes.NewReader(random), crypto.AbridgedTag)
	if err != nil {
		b.Fatal(err)
	}
	frame := bytes.Repeat([]byte{0xAB}, 1024)
	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = stream.Encrypt(frame)
	}
}
