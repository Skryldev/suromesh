package mtproto

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"

	"github.com/Skryldev/suromesh/crypto"
)

// ioReader aliases io.Reader for a compact signature.
type ioReader = io.Reader

// encryptIGE re-exports the IGE primitive (kept unexported in crypto usage
// for symmetry with EncryptMessageData's structure).
func encryptIGE(plain, key, iv []byte) ([]byte, error) { return crypto.EncryptIGE(plain, key, iv) }

// ErrNoAuthKey mirrors the client-side envelope error.
var ErrNoAuthKey = errors.New("mtproto: no auth key")

// EncryptServerMessageData builds a server→client style MTProto 2.0
// envelope around payload (salt|session_id|messages), using the server key
// derivation (x=8):
//
//	msg_key = SHA256(auth_key[96:128] || plaintext || padding)[8:24]
//
// This is the exact inverse of DecryptMessageData and mirrors what the
// reference server does. Production clients never call it; it exists so
// protocol tests and fake servers can produce genuine server envelopes
// (tests/rpc, tests/transport), and it is byte-verified against the
// Phase 2 roundtrip.json response_path vector.
func EncryptServerMessageData(authKey *crypto.AuthKey, payload []byte, rnd ioReader) ([]byte, error) {
	if !authKey.HasValue {
		return nil, ErrNoAuthKey
	}
	padding := 12 + (-(len(payload)+12)%16+16)%16
	plain := make([]byte, len(payload)+padding)
	copy(plain, payload)
	if _, err := rnd.Read(plain[len(payload):]); err != nil {
		return nil, err
	}

	sum := sha256.Sum256(append(append([]byte{}, authKey.Key[96:128]...), plain...))
	var msgKey [16]byte
	copy(msgKey[:], sum[8:24])
	aesKey, aesIV := CalcAESKeyIV(&authKey.Key, msgKey, false)
	cipherText, err := encryptIGE(plain, aesKey[:], aesIV[:])
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 24+len(cipherText))
	out = binary.LittleEndian.AppendUint64(out, authKey.KeyID)
	out = append(out, msgKey[:]...)
	return append(out, cipherText...), nil
}

// Advance bumps the sequence counter by n (mtprotostate.py adjusts
// _sequence by +64 / −16 on bad-msg codes 32/33). Additive to Phase 2;
// no existing behavior changes.
func (s *SeqNoGenerator) Advance(n int32) { s.seq += n }

// Current returns the current sequence value (diagnostics).
func (s *SeqNoGenerator) Current() int32 { return s.seq }
