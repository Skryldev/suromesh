package auth

import (
	"crypto/sha1"
	"fmt"
	"math/big"
)

// tlBytesOf encodes a non-negative big.Int as minimal big-endian TL string
// bytes (reference rsa.get_byte_array + TL framing).
func tlBytesOf(v *big.Int) string {
	if v.Sign() < 0 {
		return ""
	}
	return string(v.Bytes())
}

// keyFromArray converts the shared secret to the 256-byte auth key.
func keyFromArray(v *big.Int) [256]byte {
	var out [256]byte
	b := v.Bytes()
	copy(out[256-len(b):], b)
	return out
}

func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b)
	return h[:]
}

// keyDataFromNonce derives the temporary AES key/IV for steps 2–3
// (helpers.generate_key_data_from_nonce):
//
//	hash1 = SHA1(new_nonce ‖ server_nonce)
//	hash2 = SHA1(server_nonce ‖ new_nonce)
//	hash3 = SHA1(new_nonce ‖ new_nonce)
//	key   = hash1 ‖ hash2[:12]
//	iv    = hash2[12:20] ‖ hash3 ‖ new_nonce[:4]
func keyDataFromNonce(serverNonce [16]byte, newNonce [32]byte) (key, iv [32]byte) {
	h1 := sha1.Sum(concat(newNonce[:], serverNonce[:]))
	h2 := sha1.Sum(concat(serverNonce[:], newNonce[:]))
	h3 := sha1.Sum(concat(newNonce[:], newNonce[:]))

	copy(key[0:20], h1[:])
	copy(key[20:32], h2[:12])
	copy(iv[0:8], h2[12:20])
	copy(iv[8:28], h3[:])
	copy(iv[28:32], newNonce[:4])
	return key, iv
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// paddedTo16 pads to a multiple of 16 (client_DH_inner_data is encrypted
// without a length prefix; the reference relies on the TL body being
// padded by the IGE block size — the hash+body length is made a multiple
// of 16 exactly like AES.encrypt_ige's random tail).
func paddedTo16(b []byte) []byte {
	if rem := len(b) % 16; rem != 0 {
		b = append(b, make([]byte, 16-rem)...)
	}
	return b
}

// validateDH applies the reference's safety checks: g, g_a, g_b in
// (1, dh_prime-1) and within 2^1984 of the boundaries.
func validateDH(dhPrime, g, gA, gB *big.Int) error {
	one := big.NewInt(1)
	bound := new(big.Int).Sub(dhPrime, one)
	inRange := func(name string, v *big.Int) error {
		if v.Cmp(one) <= 0 || v.Cmp(bound) >= 0 {
			return fmt.Errorf("%w: %s not in (1, dh_prime-1)", ErrSecurity, name)
		}
		return nil
	}
	if err := inRange("g", g); err != nil {
		return err
	}
	if err := inRange("g_a", gA); err != nil {
		return err
	}
	if err := inRange("g_b", gB); err != nil {
		return err
	}

	safety := new(big.Int).Lsh(big.NewInt(1), 2048-64)
	lower := safety
	upper := new(big.Int).Sub(dhPrime, safety)
	inSafe := func(name string, v *big.Int) error {
		if v.Cmp(lower) < 0 || v.Cmp(upper) > 0 {
			return fmt.Errorf("%w: %s outside the 2^1984 safety range", ErrSecurity, name)
		}
		return nil
	}
	if err := inSafe("g_a", gA); err != nil {
		return err
	}
	return inSafe("g_b", gB)
}
