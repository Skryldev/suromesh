package crypto

import (
	"crypto/sha1"
	"encoding/binary"
)

// AuthKey is a 2048-bit MTProto authorization key with its derived identity
// values (crypto/authkey.py in the reference implementation):
//
//	aux_hash = SHA1(key)[0:8]  (little-endian uint64)
//	key_id   = SHA1(key)[12:20] (little-endian uint64)
type AuthKey struct {
	Key      [256]byte
	AuxHash  uint64
	KeyID    uint64
	HasValue bool
}

// NewAuthKey derives the identity fields from a 256-byte key.
func NewAuthKey(key [256]byte) *AuthKey {
	sum := sha1.Sum(key[:])
	return &AuthKey{
		Key:      key,
		AuxHash:  binary.LittleEndian.Uint64(sum[:8]),
		KeyID:    binary.LittleEndian.Uint64(sum[12:20]),
		HasValue: true,
	}
}

// CalcNewNonceHash computes new_nonce_hash{N} during the DH exchange
// (authkey.py calc_new_nonce_hash): SHA1(new_nonce_le(32) || number ||
// aux_hash)[4:20], the full 16 bytes interpreted little-endian. The value
// is an int128 on the wire (dh_gen_ok.new_nonce_hash1), NOT an int64 —
// pinned by tests/vectors/dh_helpers.json (Phase 5 correctness fix).
func (k *AuthKey) CalcNewNonceHash(newNonce [32]byte, number byte) [16]byte {
	var data [32 + 1 + 8]byte
	copy(data[:32], newNonce[:])
	data[32] = number
	binary.LittleEndian.PutUint64(data[33:], k.AuxHash)
	sum := sha1.Sum(data[:])
	var out [16]byte
	copy(out[:], sum[4:20])
	return out
}
