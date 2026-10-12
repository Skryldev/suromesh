package client

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"

	"github.com/Skryldev/suromesh/tl/gen"
)

// SRP (Secure Remote Password) for two-step verification, ported from the
// Python reference:
//
//	reference/SPlusthon/splusthon/password.py (compute_check, lines 136-194)
//	reference/SPlusthon/splusthon/password.py (helpers, lines 71-118)
//
// The algorithm is MTProto 2.0 cloud-password SRP:
//
//	hash1 = SHA256(salt1 ‖ password ‖ salt1)
//	hash2 = SHA256(salt2 ‖ hash1 ‖ salt2)
//	hash3 = PBKDF2-HMAC-SHA512(hash2, salt1, 100000)
//	x     = SHA256(salt2 ‖ hash3 ‖ salt2)
//	k     = SHA256(pad256(p) ‖ pad256(g))
//	g_x   = g^x mod p
//	a     = random 256 bytes (A = g^a mod p must pass is_good_mod_exp_first)
//	u     = SHA256(pad256(A) ‖ pad256(B))
//	S     = (B − k·g^x)^(a + u·x) mod p
//	M1    = SHA256(xor(SHA256(pad256(p)), SHA256(pad256(g))),
//	             SHA256(salt1), SHA256(salt2), pad256(A), pad256(B), SHA256(pad256(S)))
//
// Security properties: the password exists only inside computeCheck's
// frame and is never retained, logged, or placed in errors. Server
// parameters (p, g, B) are validated before any password material is
// processed (password.py:147-153). Go cannot guarantee memory erasure —
// documented limitation (docs/security.md).

// srpSizeForHash matches SIZE_FOR_HASH = 256 (password.py:75).
const srpSizeForHash = 256

// srpPBKDF2Iterations matches the reference's hardcoded 100000
// (password.py:117). Never lower this in production.
const srpPBKDF2Iterations = 100000

// ErrSRPParams reports a server-provided SRP parameter set that fails the
// reference's validation. Rejected before password material is touched.
var ErrSRPParams = errors.New("auth: bad SRP server parameters")

// ErrSRPAlgo reports an unsupported password KDF algorithm.
var ErrSRPAlgo = errors.New("auth: unsupported password algorithm")

// srpGoodPrime is the 2048-bit MTProto modulus the reference treats as
// pre-validated (password.py:46-62). When p equals it, the expensive
// primality proof is skipped for g ∈ {3,4,5,7}.
var srpGoodPrime = mustBig256(
	0xC7, 0x1C, 0xAE, 0xB9, 0xC6, 0xB1, 0xC9, 0x04, 0x8E, 0x6C, 0x52, 0x2F, 0x70, 0xF1, 0x3F, 0x73,
	0x98, 0x0D, 0x40, 0x23, 0x8E, 0x3E, 0x21, 0xC1, 0x49, 0x34, 0xD0, 0x37, 0x56, 0x3D, 0x93, 0x0F,
	0x48, 0x19, 0x8A, 0x0A, 0xA7, 0xC1, 0x40, 0x58, 0x22, 0x94, 0x93, 0xD2, 0x25, 0x30, 0xF4, 0xDB,
	0xFA, 0x33, 0x6F, 0x6E, 0x0A, 0xC9, 0x25, 0x13, 0x95, 0x43, 0xAE, 0xD4, 0x4C, 0xCE, 0x7C, 0x37,
	0x20, 0xFD, 0x51, 0xF6, 0x94, 0x58, 0x70, 0x5A, 0xC6, 0x8C, 0xD4, 0xFE, 0x6B, 0x6B, 0x13, 0xAB,
	0xDC, 0x97, 0x46, 0x51, 0x29, 0x69, 0x32, 0x84, 0x54, 0xF1, 0x8F, 0xAF, 0x8C, 0x59, 0x5F, 0x64,
	0x24, 0x77, 0xFE, 0x96, 0xBB, 0x2A, 0x94, 0x1D, 0x5B, 0xCD, 0x1D, 0x4A, 0xC8, 0xCC, 0x49, 0x88,
	0x07, 0x08, 0xFA, 0x9B, 0x37, 0x8E, 0x3C, 0x4F, 0x3A, 0x90, 0x60, 0xBE, 0xE6, 0x7C, 0xF9, 0xA4,
	0xA4, 0xA6, 0x95, 0x81, 0x10, 0x51, 0x90, 0x7E, 0x16, 0x27, 0x53, 0xB5, 0x6B, 0x0F, 0x6B, 0x41,
	0x0D, 0xBA, 0x74, 0xD8, 0xA8, 0x4B, 0x2A, 0x14, 0xB3, 0x14, 0x4E, 0x0E, 0xF1, 0x28, 0x47, 0x54,
	0xFD, 0x17, 0xED, 0x95, 0x0D, 0x59, 0x65, 0xB4, 0xB9, 0xDD, 0x46, 0x58, 0x2D, 0xB1, 0x17, 0x8D,
	0x16, 0x9C, 0x6B, 0xC4, 0x65, 0xB0, 0xD6, 0xFF, 0x9C, 0xA3, 0x92, 0x8F, 0xEF, 0x5B, 0x9A, 0xE4,
	0xE4, 0x18, 0xFC, 0x15, 0xE8, 0x3E, 0xBE, 0xA0, 0xF8, 0x7F, 0xA9, 0xFF, 0x5E, 0xED, 0x70, 0x05,
	0x0D, 0xED, 0x28, 0x49, 0xF4, 0x7B, 0xF9, 0x59, 0xD9, 0x56, 0x85, 0x0C, 0xE9, 0x29, 0x85, 0x1F,
	0x0D, 0x81, 0x15, 0xF6, 0x35, 0xB1, 0x05, 0xEE, 0x2E, 0x4E, 0x15, 0xD0, 0x4B, 0x24, 0x54, 0xBF,
	0x6F, 0x4F, 0xAD, 0xF0, 0x34, 0xB1, 0x04, 0x03, 0x11, 0x9C, 0xD8, 0xE3, 0xB9, 0x2F, 0xCC, 0x5B,
)

func mustBig256(bytes ...byte) *big.Int {
	if len(bytes) != 256 {
		panic("srp: prime literal must be 256 bytes")
	}
	return new(big.Int).SetBytes(bytes)
}

// srpSHA256 mirrors password.py:86-90 (multi-part SHA-256).
func srpSHA256(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

// srpNumForHash left-pads to 256 bytes (password.py:78-79);
// srpBigNumForHash mirrors big_num_for_hash (password.py:82-83).
func srpNumForHash(b []byte) []byte {
	out := make([]byte, srpSizeForHash)
	copy(out[srpSizeForHash-len(b):], b)
	return out
}

func srpBigNumForHash(g int32) []byte {
	return srpNumForHash(minimalBigEndian(int64(g)))
}

// minimalBigEndian is the minimal big-endian encoding of a non-negative
// value (Python int.to_bytes semantics after the reference's int
// conversions of g).
func minimalBigEndian(v int64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var buf [8]byte
	n := 0
	for x := v; x > 0; x >>= 8 {
		buf[7-n] = byte(x)
		n++
	}
	return buf[8-n:]
}

// srpXor mirrors password.py:105-106 (fixed 32-byte digests in use).
func srpXor(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// srpComputeHash mirrors compute_hash (password.py:113-118) with the
// production iteration count.
func srpComputeHash(salt1, salt2 []byte, password string) []byte {
	return srpComputeHashIters(salt1, salt2, password, srpPBKDF2Iterations)
}

func srpComputeHashIters(salt1, salt2 []byte, password string, iterations int) []byte {
	pw := []byte(password)
	hash1 := srpSHA256(salt1, pw, salt1)
	hash2 := srpSHA256(salt2, hash1, salt2)
	// PBKDF2-HMAC-SHA512 with a 64-byte key (password.py:109-110, 117).
	// Implemented on the standard library (hmac/sha512) to keep the crypto
	// stack dependency-free; correctness is pinned by Python-generated
	// golden vectors (tests/vectors/auth_golden.json).
	hash3 := pbkdf2SHA512(hash2, salt1, iterations, 64)
	return srpSHA256(salt2, hash3, salt2)
}

// pbkdf2SHA512 is PBKDF2 (RFC 2898/8018) with HMAC-SHA512 as the PRF.
func pbkdf2SHA512(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha512.New, password)
	hLen := sha512.Size
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	buf := make([]byte, 4)
	for block := 1; block <= numBlocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// srpIsGoodLarge mirrors is_good_large (password.py:71-72): 0 < B < p.
func srpIsGoodLarge(b, p *big.Int) bool {
	return b.Sign() > 0 && p.Cmp(b) > 0
}

// srpIsGoodModExpFirst mirrors is_good_mod_exp_first (password.py:93-102):
// both (p − modexp) and modexp must be within 64 bits of 2048, and modexp
// must fit in ≤ 256 bytes.
func srpIsGoodModExpFirst(modexp, p *big.Int) bool {
	const minDiffBits = 2048 - 64
	const maxModExpBytes = 256
	diff := new(big.Int).Sub(p, modexp)
	return diff.Sign() >= 0 &&
		diff.BitLen() >= minDiffBits &&
		modexp.BitLen() >= minDiffBits &&
		(modexp.BitLen()+7)/8 <= maxModExpBytes
}

// srpCheckPrimeAndGood mirrors check_prime_and_good (password.py:45-68).
// The reference proves primality by trial factorization (Pollard's rho,
// password.py:8-40 where factorize(p)[0]==1 ⟺ prime); Go uses
// Miller-Rabin, which decides the identical predicate for the same input.
func srpCheckPrimeAndGood(primeBytes []byte, g int32) error {
	p := new(big.Int).SetBytes(primeBytes)
	if p.Cmp(srpGoodPrime) == 0 {
		switch g {
		case 3, 4, 5, 7:
			return nil // the standard pair is pre-validated (password.py:64-66)
		}
		return fmt.Errorf("bad g %d for the standard prime", g)
	}
	if p.Sign() < 0 || p.BitLen() != 2048 {
		return fmt.Errorf("bad prime count %d, expected 2048", p.BitLen())
	}
	if !p.ProbablyPrime(64) {
		return errors.New(`given "prime" is not prime`)
	}
	mod := func(m int64) int64 {
		return new(big.Int).Mod(p, big.NewInt(m)).Int64()
	}
	switch g {
	case 2:
		if mod(8) != 7 {
			return fmt.Errorf("bad g %d, mod8 %d", g, mod(8))
		}
	case 3:
		if mod(3) != 2 {
			return fmt.Errorf("bad g %d, mod3 %d", g, mod(3))
		}
	case 4:
		// no constraint (password.py:24-25)
	case 5:
		if m := mod(5); m != 1 && m != 4 {
			return fmt.Errorf("bad g %d, mod5 %d", g, m)
		}
	case 6:
		if m := mod(24); m != 19 && m != 23 {
			return fmt.Errorf("bad g %d, mod24 %d", g, m)
		}
	case 7:
		if m := mod(7); m != 3 && m != 5 && m != 6 {
			return fmt.Errorf("bad g %d, mod7 %d", g, m)
		}
	default:
		return fmt.Errorf("bad g %d", g)
	}
	pSub1Div2 := new(big.Int).Rsh(new(big.Int).Sub(p, big.NewInt(1)), 1)
	if !pSub1Div2.ProbablyPrime(64) {
		return errors.New("(prime - 1) // 2 is not prime")
	}
	return nil
}

// srpRandomSource is replaceable so differential tests can pin the
// otherwise-random `a` value (the reference's os.urandom, password.py:166).
var srpRandomSource = rand.Reader //nolint:gochecknoglobals — test seam; production is crypto/rand

// computeCheck mirrors password.compute_check (password.py:136-194).
// iterations is the PBKDF2 round count: always srpPBKDF2Iterations in
// production; the internal test seam lowers it for fuzz throughput only.
func computeCheck(pwd *gen.AccountPassword, password string, iterations int) (*gen.InputCheckPasswordSRP, error) {
	if pwd == nil {
		return nil, errors.New("auth: nil account password")
	}
	algo, ok := pwd.CurrentAlgo.(*gen.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow)
	if !ok {
		return nil, fmt.Errorf("%w: %T", ErrSRPAlgo, pwd.CurrentAlgo)
	}
	if pwd.SRPID == nil || pwd.SRPB == nil {
		return nil, fmt.Errorf("%w: missing srp_id/srp_B (no current password?)", ErrSRPParams)
	}

	salt1, salt2 := algo.Salt1, algo.Salt2
	g := algo.G
	p := new(big.Int).SetBytes(algo.P)
	B := new(big.Int).SetBytes(pwd.SRPB)

	// password.py:147-153 — validate parameters BEFORE password work.
	if err := srpCheckPrimeAndGood(algo.P, g); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSRPParams, err)
	}
	if !srpIsGoodLarge(B, p) {
		return nil, fmt.Errorf("%w: bad b in check", ErrSRPParams)
	}

	// x = int(compute_hash(algo, password)) (password.py:142, 155)
	x := new(big.Int).SetBytes(srpComputeHashIters(salt1, salt2, password, iterations))

	pForHash := srpNumForHash(algo.P)   // num_bytes_for_hash(algo.p) — p is exactly 256B here
	gForHash := srpBigNumForHash(g)     // big_num_for_hash(g)
	bForHash := srpNumForHash(pwd.SRPB) // num_bytes_for_hash(request.srp_B)

	gx := new(big.Int).Exp(big.NewInt(int64(g)), x, p)
	k := new(big.Int).SetBytes(srpSHA256(pForHash, gForHash))
	kgx := new(big.Int).Mod(new(big.Int).Mul(k, gx), p)

	// generate_and_check_random (password.py:163-173): retry until A is
	// "good" and u > 0 (both hold with overwhelming probability per draw).
	var aInt *big.Int
	var aForHash []byte
	var u *big.Int
	for {
		random := make([]byte, 256)
		if _, err := srpRandomSource.Read(random); err != nil {
			return nil, fmt.Errorf("auth: srp randomness: %w", err)
		}
		aInt = new(big.Int).SetBytes(random)
		A := new(big.Int).Exp(big.NewInt(int64(g)), aInt, p)
		if !srpIsGoodModExpFirst(A, p) {
			continue
		}
		aForHash = srpNumForHash(A.Bytes())
		u = new(big.Int).SetBytes(srpSHA256(aForHash, bForHash))
		if u.Sign() > 0 {
			break
		}
	}

	// g_b = (B − k·g^x) mod p, validated (password.py:176-178)
	gb := new(big.Int).Mod(new(big.Int).Sub(B, kgx), p)
	if !srpIsGoodModExpFirst(gb, p) {
		return nil, fmt.Errorf("%w: bad g_b", ErrSRPParams)
	}

	// S = g_b^(a + u·x) mod p; K = SHA256(pad256(S)) (password.py:180-183)
	ux := new(big.Int).Mul(u, x)
	aUx := new(big.Int).Add(aInt, ux)
	S := new(big.Int).Exp(gb, aUx, p)
	K := srpSHA256(srpNumForHash(S.Bytes()))

	// M1 (password.py:184-191)
	M1 := srpSHA256(
		srpXor(srpSHA256(pForHash), srpSHA256(gForHash)),
		srpSHA256(salt1),
		srpSHA256(salt2),
		aForHash,
		bForHash,
		K,
	)

	return &gen.InputCheckPasswordSRP{
		SRPID: *pwd.SRPID,
		A:     aForHash,
		M1:    M1,
	}, nil
}

// constantTimeEqual documents the intent wherever password-derived or
// server-secret material must be compared (subtle.ConstantTimeCompare).
