// Package rsakeys implements the Soroush Plus RSA key exchange exactly as
// the reference implementation does (crypto/rsa.py):
//
//   - a registry of server public keys keyed by fingerprint, where the
//     fingerprint is the LAST 8 bytes (little-endian int64) of
//     SHA1(tl_bytes(n) || tl_bytes(e));
//   - encryption of a payload as the fixed 255-byte block
//     SHA1(data) || data || random(235-len(data)), raised to e modulo n —
//     NO PKCS#1 padding (protocol-required; do not "fix" this).
//
// Big-endian integer encoding (get_byte_array) is used for n, e and the
// exchanged DH quantities, unlike the little-endian TL wire format.
package rsakeys

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
)

// Errors.
var (
	// ErrUnknownFingerprint is returned when no registered key matches.
	ErrUnknownFingerprint = errors.New("rsakeys: no server key for fingerprint")
	// ErrPayloadTooLarge is returned when data does not fit the 255-byte
	// padded block (len(data) > 235).
	ErrPayloadTooLarge = errors.New("rsakeys: payload exceeds RSA block capacity")
	// ErrBadPEM is returned for unparseable key material.
	ErrBadPEM = errors.New("rsakeys: bad PEM key")
)

// BlockSize is the RSA modulus/block size in bytes (2048-bit keys).
const BlockSize = 256

// PaddedPrefixLen is SHA1 length; data capacity is 235 bytes.
const (
	padSHA1Len = 20
	dataCap    = 235
)

// PublicKey is a registered server key with its computed fingerprint.
type PublicKey struct {
	N, E        *big.Int
	Fingerprint int64
}

// keySet is the fingerprint → key registry. The embedded defaults are
// installed at init; AddKey exists for test doubles only.
type keySet struct {
	mu   sync.RWMutex
	keys map[int64]*PublicKey
}

var defaultSet = &keySet{keys: make(map[int64]*PublicKey, 24)}

// Fingerprint computes the reference fingerprint of a modulus/exponent:
// last 8 bytes of SHA1(TL-framed(n_be) || TL-framed(e_be)) as a
// little-endian signed int64.
func Fingerprint(n, e *big.Int) int64 {
	nb := getByteArray(n)
	eb := getByteArray(e)
	sum := sha1.Sum(append(tlFrame(nb), tlFrame(eb)...))
	return int64(binary.LittleEndian.Uint64(sum[12:20]))
}

// getByteArray encodes a non-negative integer as minimal big-endian bytes
// (reference rsa.get_byte_array).
func getByteArray(v *big.Int) []byte {
	if v.Sign() < 0 {
		return nil
	}
	b := v.Bytes() // big.Int.Bytes is already minimal big-endian
	return b
}

// tlFrame applies the TL string framing used in the fingerprint hash
// (TLObject.serialize_bytes with big-endian data): 1-byte length when
// < 254, zero padding to a 4-byte boundary.
func tlFrame(b []byte) []byte {
	l := len(b)
	out := make([]byte, 0, l+5)
	if l < 254 {
		out = append(out, byte(l))
		out = append(out, b...)
		if pad := (l + 1) % 4; pad != 0 {
			out = append(out, make([]byte, 4-pad)...)
		}
		return out
	}
	out = append(out, 0xFE, byte(l), byte(l>>8), byte(l>>16))
	out = append(out, b...)
	if pad := l % 4; pad != 0 {
		out = append(out, make([]byte, 4-pad)...)
	}
	return out
}

// AddKey registers a DER "BEGIN RSA PUBLIC KEY" PEM. Test doubles only —
// production key material is the embedded set installed at init.
func AddKey(pem string) (int64, error) {
	n, e, err := parsePKCS1Pub(pem)
	if err != nil {
		return 0, err
	}
	k := &PublicKey{N: n, E: e, Fingerprint: Fingerprint(n, e)}
	defaultSet.mu.Lock()
	defaultSet.keys[k.Fingerprint] = k
	defaultSet.mu.Unlock()
	return k.Fingerprint, nil
}

// Lookup returns the key registered under a fingerprint.
func Lookup(fp int64) (*PublicKey, bool) {
	defaultSet.mu.RLock()
	k, ok := defaultSet.keys[fp]
	defaultSet.mu.RUnlock()
	return k, ok
}

// Fingerprints lists every registered fingerprint (diagnostics/tests).
func Fingerprints() []int64 {
	defaultSet.mu.RLock()
	defer defaultSet.mu.RUnlock()
	out := make([]int64, 0, len(defaultSet.keys))
	for fp := range defaultSet.keys {
		out = append(out, fp)
	}
	return out
}

// Encrypt builds the reference padded block and performs the raw modexp.
// rnd supplies the random padding (crypto/rand in production; deterministic
// readers in tests only — production never weakens this).
func Encrypt(fp int64, data []byte, rnd interface {
	Read(p []byte) (int, error)
}) ([]byte, error) {
	k, ok := Lookup(fp)
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownFingerprint, fp)
	}
	if len(data) > dataCap {
		return nil, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, len(data))
	}
	block := make([]byte, 0, BlockSize)
	h := sha1.Sum(data)
	block = append(block, h[:]...)
	block = append(block, data...)
	padLen := 235 - len(data)
	pad := make([]byte, padLen)
	if _, err := rnd.Read(pad); err != nil {
		return nil, fmt.Errorf("rsakeys: padding entropy: %w", err)
	}
	block = append(block, pad...)

	m := new(big.Int).SetBytes(block)
	if m.Cmp(k.N) >= 0 {
		return nil, errors.New("rsakeys: padded block exceeds modulus")
	}
	c := new(big.Int).Exp(m, k.E, k.N)
	out := make([]byte, BlockSize)
	c.FillBytes(out) // left-padded to exactly 256 bytes, big-endian
	return out, nil
}

// DecryptPrivate performs the raw private operation m = c^d mod n (test
// servers only; the client never decrypts). The result is the minimal
// big-endian plaintext: a well-formed exchange block is exactly 255 bytes
// (SHA1(20) || data || padding), so the 256-byte ciphertext block decrypts
// to at most 255 significant bytes.
func DecryptPrivate(fp int64, d, n *big.Int, block []byte) ([]byte, error) {
	if len(block) != BlockSize {
		return nil, errors.New("rsakeys: block must be 256 bytes")
	}
	m := new(big.Int).Exp(new(big.Int).SetBytes(block), d, n)
	return m.Bytes(), nil
}

// parsePKCS1Pub parses a PKCS#1 "RSA PUBLIC KEY" PEM without pulling in
// encoding/pem+asn1 generics: the reference keys share one structure, so a
// strict, minimal DER walk keeps the parse tight and loud on deviation.
func parsePKCS1Pub(pem string) (*big.Int, *big.Int, error) {
	der, err := pemToDER(pem)
	if err != nil {
		return nil, nil, err
	}
	// SEQUENCE { INTEGER n, INTEGER e }
	r := &derReader{b: der}
	if err := r.expectTag(0x30); err != nil {
		return nil, nil, err
	}
	if _, err := r.readLen(); err != nil {
		return nil, nil, err
	}
	n, err := r.readInt()
	if err != nil {
		return nil, nil, err
	}
	e, err := r.readInt()
	if err != nil {
		return nil, nil, err
	}
	if n.Sign() <= 0 || e.Sign() <= 0 {
		return nil, nil, ErrBadPEM
	}
	return n, e, nil
}

func pemToDER(pem string) ([]byte, error) {
	lines := strings.Split(strings.TrimSpace(pem), "\n")
	if len(lines) < 3 || !strings.Contains(lines[0], "BEGIN RSA PUBLIC KEY") {
		return nil, ErrBadPEM
	}
	var b64 strings.Builder
	for _, l := range lines[1 : len(lines)-1] {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "-----") {
			continue
		}
		b64.WriteString(l)
	}
	// Standard base64 with padding.
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	var group []byte
	s := b64.String()
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			break
		}
		idx := strings.IndexByte(alpha, s[i])
		if idx < 0 {
			return nil, ErrBadPEM
		}
		group = append(group, byte(idx))
		if len(group) == 4 {
			out = append(out,
				group[0]<<2|group[1]>>4,
				group[1]<<4|group[2]>>2,
				group[2]<<6|group[3])
			group = group[:0]
		}
	}
	switch len(group) {
	case 2:
		out = append(out, group[0]<<2|group[1]>>4)
	case 3:
		out = append(out, group[0]<<2|group[1]>>4, group[1]<<4|group[2]>>2)
	}
	return out, nil
}

type derReader struct {
	b   []byte
	pos int
}

func (r *derReader) expectTag(tag byte) error {
	if r.pos >= len(r.b) || r.b[r.pos] != tag {
		return fmt.Errorf("%w: expected tag %#x", ErrBadPEM, tag)
	}
	r.pos++
	return nil
}

func (r *derReader) readLen() (int, error) {
	if r.pos >= len(r.b) {
		return 0, ErrBadPEM
	}
	l := int(r.b[r.pos])
	r.pos++
	if l&0x80 != 0 {
		n := l & 0x7F
		if n == 0 || n > 4 || r.pos+n > len(r.b) {
			return 0, ErrBadPEM
		}
		l = 0
		for i := 0; i < n; i++ {
			l = l<<8 | int(r.b[r.pos])
			r.pos++
		}
	}
	return l, nil
}

func (r *derReader) readInt() (*big.Int, error) {
	if err := r.expectTag(0x02); err != nil {
		return nil, err
	}
	l, err := r.readLen()
	if err != nil {
		return nil, err
	}
	if r.pos+l > len(r.b) {
		return nil, ErrBadPEM
	}
	v := new(big.Int).SetBytes(r.b[r.pos : r.pos+l])
	r.pos += l
	return v, nil
}

// ProductionRand is the default padding source.
var ProductionRand = rand.Reader

func init() {
	for _, pem := range embeddedKeys {
		if _, err := AddKey(pem); err != nil {
			panic("rsakeys: embedded key parse failed: " + err.Error())
		}
	}
}
