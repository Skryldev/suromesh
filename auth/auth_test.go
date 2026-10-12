package auth

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/tests/vectors"
)

type dhCase struct {
	Name           string `json:"name"`
	Nonce          string `json:"nonce"`
	ServerNonce    string `json:"server_nonce"`
	NewNonce       string `json:"new_nonce"`
	AESKey         string `json:"aes_key"`
	AESIV          string `json:"aes_iv"`
	AuthKey        string `json:"auth_key"`
	NNH1           string `json:"nnh1"`
	NNH2           string `json:"nnh2"`
	Factorizations []struct {
		PQ string `json:"pq"`
		P  string `json:"p"`
		Q  string `json:"q"`
	} `json:"factorizations"`
	PQInnerData       string `json:"pq_inner_data"`
	ClientDHInner     string `json:"client_dh_inner"`
	ClientDHPadded    string `json:"client_dh_padded"`
	ClientDHEncrypted string `json:"client_dh_encrypted"`
}

// TestKeyDataFromNonceMatchesReference pins the nonce-derived AES key/iv
// against the Python reference (helpers.generate_key_data_from_nonce).
func TestKeyDataFromNonceMatchesReference(t *testing.T) {
	c := loadDHVector(t)
	var sn, nn [16]byte = arr16(t, c.ServerNonce), [16]byte{}
	// new_nonce is 32 bytes little-endian in the reference int form.
	copy(nn[:], lePad(t, c.NewNonce, 16))
	var newNonce [32]byte
	copy(newNonce[:], lePad(t, c.NewNonce, 32))

	key, iv := keyDataFromNonce(sn, newNonce)
	if !equalHex(t, key[:], c.AESKey) {
		t.Fatalf("key mismatch:\n want %x\n  got %x", vectors.Hex(c.AESKey), key)
	}
	if !equalHex(t, iv[:], c.AESIV) {
		t.Fatalf("iv mismatch:\n want %x\n  got %x", vectors.Hex(c.AESIV), iv)
	}
	_ = nn
}

// TestCalcNewNonceHashMatchesReference pins the final hash check inputs
// (the same math AuthKey.CalcNewNonceHash already proved in Phase 2, now
// exercised through the auth package's littleEndian64 read).
func TestCalcNewNonceHashMatchesReference(t *testing.T) {
	c := loadDHVector(t)
	var keyBytes [256]byte
	copy(keyBytes[:], vectors.Hex(c.AuthKey))
	ak := crypto.NewAuthKey(keyBytes)

	var newNonce [32]byte
	copy(newNonce[:], lePad(t, c.NewNonce, 32))

	if got, want := int128LE(ak.CalcNewNonceHash(newNonce, 1)).String(), c.NNH1; got != want {
		t.Fatalf("nnh1 mismatch: got %s want %s", got, want)
	}
	if got, want := int128LE(ak.CalcNewNonceHash(newNonce, 2)).String(), c.NNH2; got != want {
		t.Fatalf("nnh2 mismatch: got %s want %s", got, want)
	}
}

// int128LE interprets the 16-byte hash the way the reference does
// (int.from_bytes(..., "little", signed=True)).
func int128LE(b [16]byte) *big.Int {
	le := make([]byte, 16)
	for i := range b {
		le[i] = b[15-i]
	}
	v := new(big.Int).SetBytes(le) // big-endian of the reversed slice
	if le[0]&0x80 != 0 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 128))
	}
	return v
}

// TestFactorizeMatchesReference factors the vector semiprimes and compares
// p/q with the reference Pollard-rho output.
func TestFactorizeMatchesReference(t *testing.T) {
	c := loadDHVector(t)
	for _, f := range c.Factorizations {
		pq := bigFromInt(t, f.PQ)
		p, q, err := Factorize(pq)
		if err != nil {
			t.Fatalf("factorize %s: %v", f.PQ, err)
		}
		if p.String() != f.P || q.String() != f.Q {
			t.Fatalf("factorization mismatch for %s: got (%s,%s) want (%s,%s)",
				f.PQ, p, q, f.P, f.Q)
		}
	}
}

// TestClientDHEncryptionMatchesReference reproduces the encrypted
// client_DH_inner_data block byte-for-byte (SHA1 ‖ body, IGE). The IGE
// path pads with random bytes; the vector records the exact padded
// plaintext so the encryption replays deterministically.
func TestClientDHEncryptionMatchesReference(t *testing.T) {
	c := loadDHVector(t)
	var sn [16]byte
	copy(sn[:], lePad(t, c.ServerNonce, 16))
	var newNonce [32]byte
	copy(newNonce[:], lePad(t, c.NewNonce, 32))
	key, iv := keyDataFromNonce(sn, newNonce)

	enc, err := crypto.EncryptIGE(vectors.Hex(c.ClientDHPadded), key[:], iv[:])
	if err != nil {
		t.Fatal(err)
	}
	if !equalHex(t, enc, c.ClientDHEncrypted) {
		t.Fatalf("client DH block mismatch:\n want %x\n  got %x", vectors.Hex(c.ClientDHEncrypted), enc)
	}
}

func loadDHVector(t *testing.T) dhCase {
	t.Helper()
	f, err := vectors.Load("dh_helpers.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []dhCase
	if err := json.Unmarshal(f.Cases, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 {
		t.Fatalf("expected 1 dh case, got %d", len(cases))
	}
	return cases[0]
}

// lePad renders the decimal int as the reference's signed little-endian
// two's-complement representation of exactly n bytes
// (int.to_bytes(n, "little", signed=True)).
func lePad(t *testing.T, dec string, n int) []byte {
	t.Helper()
	v, ok := new(big.Int).SetString(dec, 10)
	if !ok {
		t.Fatalf("bad decimal %q", dec)
	}
	mod := new(big.Int).Lsh(big.NewInt(1), uint(8*n))
	v.Mod(v, mod) // two's complement for negatives
	le := make([]byte, n)
	b := v.Bytes() // big-endian minimal of the residue
	for i := 0; i < len(b) && i < n; i++ {
		le[i] = b[len(b)-1-i]
	}
	return le
}

func arr16(t *testing.T, dec string) [16]byte {
	t.Helper()
	var out [16]byte
	copy(out[:], lePad(t, dec, 16))
	return out
}

func bigFromInt(t *testing.T, dec string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(dec, 10)
	if !ok {
		t.Fatalf("bad decimal %q", dec)
	}
	return v
}

func equalHex(t *testing.T, got []byte, wantHex string) bool {
	t.Helper()
	return string(got) == string(vectors.Hex(wantHex))
}
