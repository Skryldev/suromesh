// Package cryptography implements session envelope encryption (ADR-0008):
//
//	blob = "SMB1" ‖ kek_version ‖ wrapped_dek_len ‖ wrapped_dek ‖ nonce ‖ ciphertext
//
// A fresh 32-byte Data Encryption Key (DEK) wraps each session; the DEK
// is wrapped (AES-256-GCM) by the Key Encryption Key (KEK) from the
// configured KeyProvider. The KEK never lives beside the data; key
// rotation re-wraps DEKs without touching ciphertexts; deletion destroys
// the row (crypto-shred). Tampered ciphertext or a wrong KEK fails AEAD
// authentication — typed as domain.ErrCrypto, never a silent fallback.
package cryptography

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// KeyProvider abstracts the KEK: wrap/unwrap DEKs with authenticated
// encryption and explicit key versions (rotation support). Production
// uses a KMS-backed provider; a file provider exists ONLY for
// explicitly configured development/self-hosted deployments. There is
// no silent fallback between providers.
type KeyProvider interface {
	WrapKey(ctx context.Context, plaintextDEK []byte) (wrapped []byte, keyVersion string, err error)
	UnwrapKey(ctx context.Context, wrappedDEK []byte, keyVersion string) ([]byte, error)
}

// magic marks the envelope format in the persisted blob.
var magic = [4]byte{'S', 'M', 'B', '1'}

const (
	dekSize    = 32 // AES-256
	nonceSize  = 12 // GCM standard nonce
	maxKEKVer  = 128
	maxWrapped = 128
)

// Envelope seals/unseals session blobs.
type Envelope struct {
	provider KeyProvider
}

// NewEnvelope builds the envelope over a key provider.
func NewEnvelope(p KeyProvider) *Envelope { return &Envelope{provider: p} }

// Seal encrypts plaintext into the envelope format.
func (e *Envelope) Seal(ctx context.Context, plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return nil, fmt.Errorf("%w: empty plaintext", domain.ErrCrypto)
	}
	dek := make([]byte, dekSize)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("%w: dek generation: %v", domain.ErrCrypto, err)
	}

	wrapped, keyVersion, err := e.provider.WrapKey(ctx, dek)
	if err != nil {
		return nil, fmt.Errorf("%w: wrap dek: %v", domain.ErrCrypto, err)
	}

	aead, err := aeadFor(dek)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("%w: nonce: %v", domain.ErrCrypto, err)
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, nil)

	out := make([]byte, 0, len(magic)+1+1+len(keyVersion)+2+len(wrapped)+nonceSize+len(ciphertext))
	out = append(out, magic[:]...)
	out = append(out, byte(len(keyVersion)))
	out = append(out, keyVersion...)
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(wrapped)))
	out = append(out, lenBuf[:]...)
	out = append(out, wrapped...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

// Unseal decrypts an envelope. AEAD failures (wrong KEK, tampering) are
// typed ErrCrypto failures — never a silent fallback.
func (e *Envelope) Unseal(ctx context.Context, blob []byte) ([]byte, error) {
	// magic(4) + verLen(1) + ver + wrappedLen(2) + wrapped + nonce(12) + ct
	if len(blob) < len(magic)+1+2+nonceSize {
		return nil, fmt.Errorf("%w: envelope too short", domain.ErrCrypto)
	}
	if blob[0] != magic[0] || blob[1] != magic[1] || blob[2] != magic[2] || blob[3] != magic[3] {
		return nil, fmt.Errorf("%w: bad envelope magic", domain.ErrCrypto)
	}
	pos := len(magic)
	verLen := int(blob[pos])
	pos++
	if verLen > maxKEKVer || pos+verLen+2 > len(blob) {
		return nil, fmt.Errorf("%w: envelope key version malformed", domain.ErrCrypto)
	}
	keyVersion := string(blob[pos : pos+verLen])
	pos += verLen
	wrappedLen := int(binary.BigEndian.Uint16(blob[pos : pos+2]))
	pos += 2
	if wrappedLen > maxWrapped || pos+wrappedLen+nonceSize > len(blob) {
		return nil, fmt.Errorf("%w: envelope wrapped key malformed", domain.ErrCrypto)
	}
	wrapped := blob[pos : pos+wrappedLen]
	pos += wrappedLen
	nonce := blob[pos : pos+nonceSize]
	pos += nonceSize
	ciphertext := blob[pos:]

	dek, err := e.provider.UnwrapKey(ctx, wrapped, keyVersion)
	if err != nil {
		return nil, fmt.Errorf("%w: unwrap dek: %v", domain.ErrCrypto, err)
	}
	aead, err := aeadFor(dek)
	if err != nil {
		return nil, err
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: session authentication failed (wrong key or tampered data)", domain.ErrCrypto)
	}
	return plaintext, nil
}

// ReWrap rotates the KEK: unseals the blob and reseals it under the
// CURRENT provider version with a fresh DEK and nonce (the DEK is
// re-generated, the plaintext is re-encrypted). The plaintext exists only
// in process memory for the duration of the call; the old wrapped DEK
// (and the old KEK version) is no longer needed to read the returned
// blob. Note the original ciphertext is NOT byte-identical after
// rotation — callers must persist the returned blob.
func (e *Envelope) ReWrap(ctx context.Context, blob []byte) ([]byte, error) {
	plaintext, err := e.Unseal(ctx, blob)
	if err != nil {
		return nil, err
	}
	return e.Seal(ctx, plaintext)
}

func aeadFor(dek []byte) (cipher.AEAD, error) {
	if len(dek) != dekSize {
		return nil, fmt.Errorf("%w: dek size %d, want %d", domain.ErrCrypto, len(dek), dekSize)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrCrypto, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrCrypto, err)
	}
	return gcm, nil
}
