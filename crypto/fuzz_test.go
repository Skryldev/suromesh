package crypto_test

import (
	"bytes"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
)

func TestIGERoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	iv := bytes.Repeat([]byte{9}, 32)
	plain := bytes.Repeat([]byte{1}, 48)
	ct, err := crypto.EncryptIGE(plain, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	back, err := crypto.DecryptIGE(ct, key, iv)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, plain) {
		t.Fatal("round trip mismatch")
	}
}

// FuzzIGE asserts the block-size contract: non-multiple-of-16 input is
// rejected, never panicked on.
func FuzzIGE(f *testing.F) {
	f.Add([]byte("sixteen byte blk"), make([]byte, 32), make([]byte, 32))
	f.Fuzz(func(t *testing.T, data, key, iv []byte) {
		if len(data) > 1<<20 || len(key) != 32 || len(iv) != 32 {
			return
		}
		ct, err := crypto.EncryptIGE(data, key, iv)
		if err != nil {
			if len(data)%16 != 0 {
				return // expected rejection
			}
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ct) != len(data) {
			t.Fatal("ciphertext length mismatch")
		}
		back, err := crypto.DecryptIGE(ct, key, iv)
		if err != nil || !bytes.Equal(back, data) {
			t.Fatal("round trip failed")
		}
	})
}

// FuzzObfuscated asserts header init never panics on adversarial sources and
// that the derived header always has the splice shape. Note the encrypt and
// decrypt keystreams are independent by protocol design; a same-stream
// round trip is intentionally not asserted.
func FuzzObfuscated(f *testing.F) {
	f.Add(make([]byte, 64), []byte{0xEF, 0xEF, 0xEF, 0xEF})
	f.Fuzz(func(t *testing.T, random, tag []byte) {
		if len(random) != 64 || len(tag) != 4 {
			return
		}
		header, stream, err := crypto.InitObfuscated(bytes.NewReader(random), tag)
		if err != nil {
			return
		}
		if len(header) != 64 {
			t.Fatal("header length")
		}
		sample := []byte("sample frame payload")
		enc := stream.Encrypt(sample)
		if len(enc) != len(sample) {
			t.Fatal("stream length mismatch")
		}
		// Second call must advance the keystream (continuous stream).
		enc2 := stream.Encrypt(sample)
		if bytes.Equal(enc, enc2) {
			t.Fatal("keystream did not advance")
		}
	})
}
