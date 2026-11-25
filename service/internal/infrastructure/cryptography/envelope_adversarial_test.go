package cryptography_test

// Adversarial envelope battery (Phase 27 §8.1): every malformed or
// hostile input the persistence layer could ever hand the unsealer must
// fail closed with domain.ErrCrypto — never panic, never fall back, never
// partially decrypt. Complements envelope_test.go (round-trip, tamper,
// wrong-key, rotation) with the systematic cases:
//   - truncation at EVERY boundary length
//   - modified nonce / modified wrapped DEK (authentication targets)
//   - unknown / oversized KEK version and wrapped-DEK length fields
//   - KEK provider unavailable mid-operation (fail closed, typed)
//   - key rotation compatibility across two provider instances
//   - empty and single-byte ciphertexts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/infrastructure/cryptography"
)

func newTestEnvelope(tb testing.TB) (*cryptography.Envelope, *cryptography.FileKeyProvider) {
	tb.Helper()
	p, err := cryptography.NewFileKeyProvider(testKeyFile(tb))
	if err != nil {
		tb.Fatal(err)
	}
	return cryptography.NewEnvelope(p), p
}

func sealTest(tb testing.TB, env *cryptography.Envelope) []byte {
	tb.Helper()
	blob, err := env.Seal(context.Background(), []byte("session state material"))
	if err != nil {
		tb.Fatal(err)
	}
	return blob
}

func wantCryptoErr(t *testing.T, blob []byte, env *cryptography.Envelope, what string) {
	t.Helper()
	got, err := env.Unseal(context.Background(), blob)
	if err == nil {
		t.Fatalf("%s: unseal unexpectedly SUCCEEDED (got %d bytes)", what, len(got))
	}
	if !errors.Is(err, domain.ErrCrypto) {
		t.Fatalf("%s: err = %v (%T), want domain.ErrCrypto", what, err, err)
	}
}

// TestEnvelope_AdversarialTruncation: every truncation from 0 bytes to the
// full length must fail closed (never index-panic, never succeed).
func TestEnvelope_AdversarialTruncation(t *testing.T) {
	env, _ := newTestEnvelope(t)
	blob := sealTest(t, env)
	for cut := 0; cut <= len(blob); cut++ {
		if cut == len(blob) {
			continue // the intact blob is the control case
		}
		wantCryptoErr(t, blob[:cut], env, fmt.Sprintf("truncated to %d/%d bytes", cut, len(blob)))
	}
	// Control: intact blob unseals.
	if _, err := env.Unseal(context.Background(), blob); err != nil {
		t.Fatalf("control unseal failed: %v", err)
	}
}

// TestEnvelope_AdversarialByteFlip: EVERY single-byte flip anywhere in the
// envelope must fail closed (magic, version length, version, wrapped-DEK
// length, wrapped DEK, nonce, ciphertext).
func TestEnvelope_AdversarialByteFlip(t *testing.T) {
	env, _ := newTestEnvelope(t)
	blob := sealTest(t, env)
	for i := 0; i < len(blob); i++ {
		bad := append([]byte{}, blob...)
		bad[i] ^= 0xFF
		wantCryptoErr(t, bad, env, fmt.Sprintf("byte flip at %d", i))
	}
}

// TestEnvelope_MalformedLengthFields: hostile length fields (oversized
// version length / wrapped-DEK length) must fail closed without reading
// out of bounds.
func TestEnvelope_MalformedLengthFields(t *testing.T) {
	env, _ := newTestEnvelope(t)
	blob := sealTest(t, env)

	// verLen is at offset 4. Force absurd sizes.
	for _, vl := range []byte{200, 255} {
		bad := append([]byte{}, blob...)
		bad[4] = vl
		wantCryptoErr(t, bad, env, fmt.Sprintf("verLen=%d", vl))
	}
	// wrappedLen is the 2 bytes right after the version string
	// (4 + 1 + verLen). Seal's version is "file:<16hex>" — locate it.
	verLen := int(blob[4])
	wlPos := 5 + verLen
	for _, wl := range []int{0, 1, 129, 255, 256, 0xFFFF} {
		bad := append([]byte{}, blob...)
		bad[wlPos] = byte(wl >> 8)
		bad[wlPos+1] = byte(wl)
		wantCryptoErr(t, bad, env, fmt.Sprintf("wrappedLen=%d", wl))
	}
}

// TestEnvelope_UnknownKEKVersion: a blob whose embedded key version does
// not match the provider's version must fail closed (unwrap targets a
// version-bound HMAC domain).
func TestEnvelope_UnknownKEKVersion(t *testing.T) {
	envA, _ := newTestEnvelope(t)
	blob := sealTest(t, envA)

	// Rewrite the version bytes in place, keeping lengths consistent:
	// flip the first version byte to make it an unknown version string.
	verLen := int(blob[4])
	bad := append([]byte{}, blob...)
	bad[5] ^= 0x01
	_ = verLen
	wantCryptoErr(t, bad, envA, "unknown kek version")
}

// failingProvider simulates an unavailable KEK source (KMS outage, file
// permissions, network death). KeyProvider is the production interface —
// the envelope must treat provider failures as typed crypto failures.
type failingProvider struct{}

var _ cryptography.KeyProvider = failingProvider{}

func (failingProvider) WrapKey(ctx context.Context, plaintextDEK []byte) ([]byte, string, error) {
	return nil, "", errors.New("KEK source unavailable (simulated outage)")
}
func (failingProvider) UnwrapKey(ctx context.Context, wrappedDEK []byte, keyVersion string) ([]byte, error) {
	return nil, errors.New("KEK source unavailable (simulated outage)")
}

// TestEnvelope_ProviderUnavailableFailsClosed: a failing KEK provider must
// produce typed ErrCrypto on Seal and Unseal — never a silent fallback or
// a panic (KMS outage behavior).
func TestEnvelope_ProviderUnavailableFailsClosed(t *testing.T) {
	ctx := context.Background()
	env := cryptography.NewEnvelope(failingProvider{})
	if _, err := env.Seal(ctx, []byte("x")); !errors.Is(err, domain.ErrCrypto) {
		t.Fatalf("seal with dead provider: err = %v, want ErrCrypto", err)
	}
	env2, _ := newTestEnvelope(t)
	blob := sealTest(t, env2)
	deadEnv := cryptography.NewEnvelope(failingProvider{})
	if _, err := deadEnv.Unseal(ctx, blob); !errors.Is(err, domain.ErrCrypto) {
		t.Fatalf("unseal with dead provider: err = %v, want ErrCrypto", err)
	}
}

// TestEnvelope_RotationSemantics: pins the EXACT rotation semantics.
//   - ReWrap rotates WITHIN one provider (same KEK, fresh DEK+nonce);
//     cross-provider "rewrap" must fail closed (a different KEK can never
//     unwrap a foreign blob — no silent fallback).
//   - Cross-provider KEK migration is the caller-choreographed
//     Unseal(old) → Seal(new); the migrated blob round-trips under the
//     new key and the old key can no longer read it.
func TestEnvelope_RotationSemantics(t *testing.T) {
	envA, _ := newTestEnvelope(t)
	// Provider B: different key material.
	pathB := filepath.Join(t.TempDir(), "kek-b.bin")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte((i*13 + 5) % 251)
	}
	if err := os.WriteFile(pathB, key, 0o600); err != nil {
		t.Fatal(err)
	}
	pB, err := cryptography.NewFileKeyProvider(pathB)
	if err != nil {
		t.Fatal(err)
	}
	envB := cryptography.NewEnvelope(pB)
	ctx := context.Background()

	plain := []byte("rotation payload")
	blobA, err := envA.Seal(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	// Cross-key isolation: B cannot read A's blob.
	wantCryptoErr(t, blobA, envB, "B reading A's blob")
	// And ReWrap under B must fail closed for a foreign blob (it is
	// Unseal+Seal with B's provider — the unwrap correctly fails).
	if _, err := envB.ReWrap(ctx, blobA); !errors.Is(err, domain.ErrCrypto) {
		t.Fatalf("cross-provider ReWrap = %v, want ErrCrypto (no silent fallback)", err)
	}

	// Same-provider rotation works and round-trips (fresh DEK+nonce).
	rotatedA, err := envA.ReWrap(ctx, blobA)
	if err != nil {
		t.Fatalf("same-provider rewrap: %v", err)
	}
	got, err := envA.Unseal(ctx, rotatedA)
	if err != nil {
		t.Fatalf("unseal rotated: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatal("rotated payload mismatch")
	}

	// Cross-provider migration = Unseal(old) → Seal(new).
	migrated, err := envB.Seal(ctx, func() []byte {
		p, err := envA.Unseal(ctx, blobA)
		if err != nil {
			t.Fatalf("migration unseal: %v", err)
		}
		return p
	}())
	if err != nil {
		t.Fatalf("migration seal: %v", err)
	}
	got, err = envB.Unseal(ctx, migrated)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("migrated blob round trip: %v", err)
	}
	// A can no longer read the migrated blob.
	wantCryptoErr(t, migrated, envA, "A reading migrated blob")
}
