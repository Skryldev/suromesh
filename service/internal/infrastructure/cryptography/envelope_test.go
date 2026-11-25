package cryptography_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/infrastructure/cryptography"
)

func testKeyFile(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kek.bin")
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i * 7 % 251)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvelope_SealUnsealRoundTrip(t *testing.T) {
	p, err := cryptography.NewFileKeyProvider(testKeyFile(t))
	if err != nil {
		t.Fatal(err)
	}
	env := cryptography.NewEnvelope(p)
	ctx := context.Background()

	plaintext := []byte(`{"version":1,"auth_key_b64":"...","authorized":true}`)
	blob, err := env.Seal(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("auth_key")) {
		t.Fatal("plaintext leaked into envelope")
	}
	got, err := env.Unseal(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("round trip mismatch")
	}
}

func TestEnvelope_TamperFailsClosed(t *testing.T) {
	p, _ := cryptography.NewFileKeyProvider(testKeyFile(t))
	env := cryptography.NewEnvelope(p)
	ctx := context.Background()

	blob, err := env.Seal(ctx, []byte("session state material"))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 4, len(blob) - 1} { // magic, wrapped dek, ciphertext
		bad := append([]byte{}, blob...)
		bad[i] ^= 0x01
		if _, err := env.Unseal(ctx, bad); !errors.Is(err, domain.ErrCrypto) {
			t.Fatalf("tampered byte %d: err = %v, want ErrCrypto", i, err)
		}
	}
}

func TestEnvelope_WrongKeyFailsClosed(t *testing.T) {
	p1, _ := cryptography.NewFileKeyProvider(testKeyFile(t))
	env1 := cryptography.NewEnvelope(p1)
	ctx := context.Background()
	blob, err := env1.Seal(ctx, []byte("secret state"))
	if err != nil {
		t.Fatal(err)
	}
	// A different key file → different KEK.
	key2 := make([]byte, 32)
	path2 := filepath.Join(t.TempDir(), "kek2.bin")
	for i := range key2 {
		key2[i] = byte(255 - i)
	}
	if err := os.WriteFile(path2, key2, 0o600); err != nil {
		t.Fatal(err)
	}
	p2, _ := cryptography.NewFileKeyProvider(path2)
	env2 := cryptography.NewEnvelope(p2)
	if _, err := env2.Unseal(ctx, blob); !errors.Is(err, domain.ErrCrypto) {
		t.Fatalf("wrong-key unseal err = %v, want ErrCrypto", err)
	}
}

func TestEnvelope_RotationReWrap(t *testing.T) {
	oldP, _ := cryptography.NewFileKeyProvider(testKeyFile(t))
	envOld := cryptography.NewEnvelope(oldP)
	ctx := context.Background()
	plaintext := []byte("state to survive rotation")
	blob, err := envOld.Seal(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}

	// New key file (rotation): re-wrap the DEK without the old KEK being
	// able to open the new envelope.
	key2 := make([]byte, 32)
	path2 := filepath.Join(t.TempDir(), "kek2.bin")
	for i := range key2 {
		key2[i] = byte(255 - i)
	}
	if err := os.WriteFile(path2, key2, 0o600); err != nil {
		t.Fatal(err)
	}
	newP, _ := cryptography.NewFileKeyProvider(path2)
	envNew := cryptography.NewEnvelope(newP)

	// Re-wrap requires decrypting with the old provider first (the
	// documented rotation procedure: unseal with old → seal with new).
	plain, err := envOld.Unseal(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := envNew.Seal(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := envNew.Unseal(ctx, rotated)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatal("rotation round trip mismatch")
	}
	// The old envelope remains sealed under the old key version (key
	// versions are recorded per envelope).
	if _, err := envNew.Unseal(ctx, blob); !errors.Is(err, domain.ErrCrypto) {
		t.Fatal("old envelope must not open under the new KEK")
	}
}

func TestFileKeyProvider_Errors(t *testing.T) {
	if _, err := cryptography.NewFileKeyProvider(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, domain.ErrCrypto) {
		t.Fatalf("missing file err = %v", err)
	}
	short := filepath.Join(t.TempDir(), "short.bin")
	if err := os.WriteFile(short, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cryptography.NewFileKeyProvider(short); !errors.Is(err, domain.ErrCrypto) {
		t.Fatalf("short key err = %v", err)
	}
}

func TestKMSProvider_FailsClosed(t *testing.T) {
	if _, err := cryptography.NewKMSProvider(); !errors.Is(err, domain.ErrCrypto) {
		t.Fatal("KMS provider must fail closed (owner decision pending)")
	}
}

func TestEnvelope_EmptyPlaintextRejected(t *testing.T) {
	p, _ := cryptography.NewFileKeyProvider(testKeyFile(t))
	env := cryptography.NewEnvelope(p)
	if _, err := env.Seal(context.Background(), nil); !errors.Is(err, domain.ErrCrypto) {
		t.Fatal("empty plaintext must be rejected")
	}
}
