package cryptography

// Key providers. The file provider is for EXPLICITLY configured
// development/self-hosted deployments only; production must configure a
// KMS-backed provider. There is no silent fallback: an unconfigured or
// unavailable provider fails startup (fail closed).

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// FileKeyProvider reads a 32-byte KEK from a file at construction (NOT
// per operation). Key version: "file:<sha256:16hex-of-kek>" — a stable
// identifier that changes when the key file changes (rotation visibility)
// without exposing the key itself.
type FileKeyProvider struct {
	kek        [32]byte
	keyVersion string
}

var _ KeyProvider = (*FileKeyProvider)(nil)

// NewFileKeyProvider loads the KEK from path. The file must be exactly
// 32 bytes (raw key) with restrictive permissions recommended.
func NewFileKeyProvider(path string) (*FileKeyProvider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read key file: %v", domain.ErrCrypto, err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: key file %s must be exactly 32 bytes, got %d",
			domain.ErrCrypto, path, len(raw))
	}
	p := &FileKeyProvider{}
	copy(p.kek[:], raw)
	sum := sha256.Sum256(raw)
	p.keyVersion = fmt.Sprintf("file:%x", sum[:8])
	return p, nil
}

func (p *FileKeyProvider) aead() (cipher.AEAD, string, error) {
	block, err := aes.NewCipher(p.kek[:])
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", domain.ErrCrypto, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", domain.ErrCrypto, err)
	}
	return gcm, p.keyVersion, nil
}

// WrapKey seals the DEK with the file KEK. The file KEK is itself bound
// to the key version via HMAC (tamper-evident unwrap targets).
func (p *FileKeyProvider) WrapKey(ctx context.Context, plaintextDEK []byte) ([]byte, string, error) {
	if len(plaintextDEK) != 32 {
		return nil, "", fmt.Errorf("%w: dek size %d, want 32", domain.ErrCrypto, len(plaintextDEK))
	}
	gcm, version, err := p.aead()
	if err != nil {
		return nil, "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", fmt.Errorf("%w: %v", domain.ErrCrypto, err)
	}
	ct := gcm.Seal(nil, nonce, plaintextDEK, []byte(version))
	// nonce ‖ ct
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, version, nil
}

// UnwrapKey opens the wrapped DEK. A wrong KEK or version mismatch fails
// AEAD authentication (typed crypto failure).
func (p *FileKeyProvider) UnwrapKey(ctx context.Context, wrapped []byte, keyVersion string) ([]byte, error) {
	gcm, _, err := p.aead()
	if err != nil {
		return nil, err
	}
	if len(wrapped) < gcm.NonceSize() {
		return nil, fmt.Errorf("%w: wrapped key too short", domain.ErrCrypto)
	}
	nonce, ct := wrapped[:gcm.NonceSize()], wrapped[gcm.NonceSize():]
	dek, err := gcm.Open(nil, nonce, ct, []byte(keyVersion))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrCrypto, err)
	}
	return dek, nil
}

// KMSProvider is the production key provider seam. The concrete
// implementation requires external credentials and an owner decision on
// the KMS vendor — deliberately unimplemented in this phase: constructing
// it fails closed rather than pretending.
type KMSProvider struct{}

var _ KeyProvider = (*KMSProvider)(nil)

// NewKMSProvider is a fail-closed placeholder: production key management
// requires an external credential + vendor decision (documented external
// blocker). It returns a typed error instead of a working default.
func NewKMSProvider() (*KMSProvider, error) {
	return nil, fmt.Errorf(
		"%w: KMS key provider requires an owner decision (vendor/credentials) — "+
			"configure crypto.provider=file for development only", domain.ErrCrypto)
}

func (p *KMSProvider) WrapKey(ctx context.Context, plaintextDEK []byte) ([]byte, string, error) {
	return nil, "", fmt.Errorf("%w: KMS provider not configured", domain.ErrCrypto)
}

func (p *KMSProvider) UnwrapKey(ctx context.Context, wrapped []byte, keyVersion string) ([]byte, error) {
	return nil, fmt.Errorf("%w: KMS provider not configured", domain.ErrCrypto)
}

// HMACKey derives the account-phone lookup hash (keyed, not plain SHA —
// prevents offline enumeration of bound phones given DB access).
func HMACKey(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
