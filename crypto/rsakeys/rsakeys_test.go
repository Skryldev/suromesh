package rsakeys_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/Skryldev/suromesh/crypto/rsakeys"
	"github.com/Skryldev/suromesh/tests/vectors"
)

type rsaCase struct {
	Name         string            `json:"name"`
	Fingerprints map[string]string `json:"fingerprints"`
	PublicPEM    string            `json:"public_pem"`
	PrivatePEM   string            `json:"private_pem"`
	Fingerprint  int64             `json:"fingerprint"`
	Payload      string            `json:"payload"`
	Plain        string            `json:"plain"`
	Block        string            `json:"block"`
}

func loadRSAVector(t *testing.T) []rsaCase {
	t.Helper()
	f, err := vectors.Load("rsa_exchange.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []rsaCase
	if err := json.Unmarshal(f.Cases, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func findCase(t *testing.T, cases []rsaCase, name string) *rsaCase {
	t.Helper()
	for i := range cases {
		if cases[i].Name == name {
			return &cases[i]
		}
	}
	t.Fatalf("vector case %s missing", name)
	return nil
}

// TestEmbeddedFingerprintsMatchReference verifies every embedded server key
// derives the exact fingerprint the Python reference computes, and that all
// 20 reference fingerprints are registered. (The count may be higher: test
// keys register into the same default set.)
func TestEmbeddedFingerprintsMatchReference(t *testing.T) {
	cases := loadRSAVector(t)
	ref := findCase(t, cases, "embedded_fingerprints").Fingerprints
	if len(ref) != 20 {
		t.Fatalf("vector carries %d fingerprints, want 20", len(ref))
	}
	for key := range ref {
		u, err := strconv.ParseUint(key, 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		fp := int64(u)
		k, found := rsakeys.Lookup(fp)
		if !found {
			t.Fatalf("fingerprint %s missing from Go registry", key)
		}
		if fmt.Sprintf("%016x", uint64(k.Fingerprint)) != key {
			t.Fatalf("fingerprint round trip mismatch for %s", key)
		}
	}
	if got := len(rsakeys.Fingerprints()); got < 20 {
		t.Fatalf("registered %d keys, want >= 20", got)
	}
}

// TestEncryptMatchesReference reproduces the reference encrypted block
// byte-for-byte: the vector's plaintext (SHA1 || payload || pad) is fed
// through Go's Encrypt with the recorded padding, and the result must
// equal the recorded 256-byte ciphertext. Production always draws padding
// from crypto/rand; the recorded pad replays determinism.
func TestEncryptMatchesReference(t *testing.T) {
	cases := loadRSAVector(t)
	vc := findCase(t, cases, "test_key_block")

	fp, err := rsakeys.AddKey(vc.PublicPEM)
	if err != nil {
		t.Fatalf("add test key: %v", err)
	}
	if fp != vc.Fingerprint {
		t.Fatalf("fingerprint mismatch: got %d want %d", fp, vc.Fingerprint)
	}

	payload := vectors.Hex(vc.Payload)
	plain := vectors.Hex(vc.Plain)
	refBlock := vectors.Hex(vc.Block)

	// The 256-byte big-endian plaintext carries a leading zero (the block
	// is 255 significant bytes): SHA1(payload) || payload || pad.
	if len(plain) != 256 || plain[0] != 0 {
		t.Fatalf("plaintext shape: %d bytes", len(plain))
	}
	body := plain[1:]
	sum := sha1.Sum(payload)
	if !bytes.Equal(body[:20], sum[:]) {
		t.Fatal("vector plaintext does not start with SHA1(payload)")
	}
	if !bytes.Equal(body[20:20+len(payload)], payload) {
		t.Fatal("vector plaintext payload segment mismatch")
	}
	pad := body[20+len(payload):]

	got, err := rsakeys.Encrypt(fp, payload, bytes.NewReader(pad))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !bytes.Equal(got, refBlock) {
		t.Fatalf("block mismatch:\n want %x\n  got %x", refBlock, got)
	}

	// Wrong fingerprint is rejected.
	if _, err := rsakeys.Encrypt(0xDEADBEEF, payload, bytes.NewReader(pad)); err == nil {
		t.Fatal("unknown fingerprint accepted")
	}
	// Oversized payload is rejected.
	if _, err := rsakeys.Encrypt(fp, bytes.Repeat([]byte{1}, 236), bytes.NewReader(pad)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

// TestPrivateRoundTrip proves the block algebra end to end: the private op
// on the recorded ciphertext recovers exactly the recorded plaintext.
func TestPrivateRoundTrip(t *testing.T) {
	cases := loadRSAVector(t)
	vc := findCase(t, cases, "test_key_block")

	n, e := pubKeyOf(t, vc.PublicPEM)
	d := privateExponentOf(t, vc.PrivatePEM, e, n)

	recovered, err := rsakeys.DecryptPrivate(vc.Fingerprint, d, n, vectors.Hex(vc.Block))
	if err != nil {
		t.Fatal(err)
	}
	want := vectors.Hex(vc.Plain)
	// Normalize both to the 255-byte significant form.
	if len(recovered) == 256 && recovered[0] == 0 {
		recovered = recovered[1:]
	}
	if len(want) == 256 && want[0] == 0 {
		want = want[1:]
	}
	if !bytes.Equal(recovered, want) {
		t.Fatal("private op does not reproduce the vector plaintext")
	}
}

// TestTamperedBlockRejected: flipping one ciphertext byte yields a
// plaintext whose SHA1 prefix no longer matches the payload — the format
// itself detects tampering (this is what the exchange's sanity checks rely
// on upstream; the auth client re-verifies nonce equality independently).
func TestTamperedBlockRejected(t *testing.T) {
	cases := loadRSAVector(t)
	vc := findCase(t, cases, "test_key_block")
	n, e := pubKeyOf(t, vc.PublicPEM)
	d := privateExponentOf(t, vc.PrivatePEM, e, n)

	block := append([]byte{}, vectors.Hex(vc.Block)...)
	block[100] ^= 0x01
	recovered, err := rsakeys.DecryptPrivate(vc.Fingerprint, d, n, block)
	if err != nil {
		t.Fatal(err)
	}
	payload := vectors.Hex(vc.Payload)
	sum := sha1.Sum(payload)
	body := recovered
	if len(body) == 256 && body[0] == 0 {
		body = body[1:]
	}
	if bytes.Equal(body[:20], sum[:]) {
		t.Fatal("tampered block still verified")
	}
}

func pubKeyOf(t *testing.T, pem string) (*big.Int, *big.Int) {
	t.Helper()
	fp, err := rsakeys.AddKey(pem)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := rsakeys.Lookup(fp)
	return k.N, k.E
}

// privateExponentOf walks the PKCS#1 private PEM for d (test-only).
func privateExponentOf(t *testing.T, pem string, e, n *big.Int) *big.Int {
	t.Helper()
	der := pemDER(t, pem)
	if der[0] != 0x30 {
		t.Fatal("bad private DER")
	}
	r := &derWalker{b: der[1:]}
	r.skipLen()
	r.skipInt() // version
	nn := r.readInt(t)
	if nn.Cmp(n) != 0 {
		t.Fatal("private n mismatch")
	}
	ee := r.readInt(t)
	if ee.Cmp(e) != 0 {
		t.Fatal("private e mismatch")
	}
	return r.readInt(t)
}

type derWalker struct {
	b []byte
}

func (w *derWalker) skipLen() {
	l := int(w.b[0])
	w.b = w.b[1:]
	if l&0x80 != 0 {
		n := l & 0x7F
		l = 0
		for i := 0; i < n; i++ {
			l = l<<8 | int(w.b[0])
			w.b = w.b[1:]
		}
	}
	_ = l // the containing sequence spans the rest
}

func (w *derWalker) skipInt() {
	if w.b[0] != 0x02 {
		panic("bad DER int tag")
	}
	w.b = w.b[1:]
	l := int(w.b[0])
	w.b = w.b[1:]
	if l&0x80 != 0 {
		n := l & 0x7F
		l = 0
		for i := 0; i < n; i++ {
			l = l<<8 | int(w.b[0])
			w.b = w.b[1:]
		}
	}
	w.b = w.b[l:]
}

func (w *derWalker) readInt(t *testing.T) *big.Int {
	if w.b[0] != 0x02 {
		t.Fatal("bad DER int tag")
	}
	w.b = w.b[1:]
	l := int(w.b[0])
	w.b = w.b[1:]
	if l&0x80 != 0 {
		n := l & 0x7F
		l = 0
		for i := 0; i < n; i++ {
			l = l<<8 | int(w.b[0])
			w.b = w.b[1:]
		}
	}
	v := new(big.Int).SetBytes(w.b[:l])
	w.b = w.b[l:]
	return v
}

func pemDER(t *testing.T, pem string) []byte {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(pem), "\n")
	var b64 strings.Builder
	for _, l := range lines[1:] {
		if strings.HasPrefix(strings.TrimSpace(l), "-----") {
			break
		}
		b64.WriteString(strings.TrimSpace(l))
	}
	out, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return out
}
