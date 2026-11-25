package cryptography_test

// Envelope benchmarks (Phase 27 §15): session encryption sits on the
// persistence hot path (every session persist/load seals/unseals a blob).
// Run inside the Docker CI environment; results recorded in
// evidence/phase27/benchmark-results.txt with the exact environment.

import (
	"bytes"
	"context"
	"testing"
)

func benchEnvelope(b *testing.B, blobSize int) {
	env, _ := newTestEnvelope(b)
	ctx := context.Background()
	plain := bytes.Repeat([]byte("session-state-"), blobSize/14)
	blob, err := env.Seal(ctx, plain)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(blobSize))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := env.Seal(ctx, plain); err != nil {
			b.Fatal(err)
		}
		if _, err := env.Unseal(ctx, blob); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnvelope_SealUnseal_1KiB(b *testing.B)  { benchEnvelope(b, 1<<10) }
func BenchmarkEnvelope_SealUnseal_64KiB(b *testing.B) { benchEnvelope(b, 64<<10) }
func BenchmarkEnvelope_SealUnseal_1MiB(b *testing.B)  { benchEnvelope(b, 1<<20) }

func BenchmarkEnvelope_ReWrap_1KiB(b *testing.B) {
	env, _ := newTestEnvelope(b)
	ctx := context.Background()
	blob, err := env.Seal(ctx, bytes.Repeat([]byte("x"), 1<<10))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := env.ReWrap(ctx, blob); err != nil {
			b.Fatal(err)
		}
	}
}
