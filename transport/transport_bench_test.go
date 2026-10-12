package transport

import (
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/transport/abridged"
)

// Benchmarks measure each transport layer in isolation. End-to-end
// loopback numbers live in tests/transport/bench_test.go.

func BenchmarkAbridgedEncode1K(b *testing.B) {
	payload := make([]byte, 1024)
	buf := make([]byte, 0, 1100)
	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		buf, err = abridged.Encode(buf[:0], payload)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAbridgedDecode1K(b *testing.B) {
	payload := make([]byte, 1024)
	frame, err := abridged.Encode(nil, payload)
	if err != nil {
		b.Fatal(err)
	}
	stream := make([]byte, 0, len(frame)*64)
	for i := 0; i < 64; i++ {
		stream = append(stream, frame...)
	}
	d := abridged.NewDecoder(0)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Feed(stream, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkObfuscate1K(b *testing.B) {
	header, streams, err := crypto.InitObfuscated(deterministicRand{}, crypto.AbridgedTag)
	if err != nil {
		b.Fatal(err)
	}
	_ = header
	frame := make([]byte, 1024)
	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = streams.Encrypt(frame)
	}
}

// deterministicRand yields a fixed 64-byte stream for the header.
type deterministicRand struct{}

func (deterministicRand) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i * 7)
	}
	return len(p), nil
}
