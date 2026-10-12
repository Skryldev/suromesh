package compatibility_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/tests/vectors"
)

// TestServerEnvelopeReproducesReference proves EncryptServerMessageData
// byte-reproduces the Python-generated server envelope from roundtrip.json
// (response_path), and that the library decrypt path opens it.
func TestServerEnvelopeReproducesReference(t *testing.T) {
	f := mustLoad(t, "roundtrip.json")
	var cases []envCase
	if err := json.Unmarshal(f.Cases, &cases); err != nil {
		t.Fatal(err)
	}
	var resp *envCase
	for i := range cases {
		if cases[i].Name == "response_path" {
			resp = &cases[i]
		}
	}
	if resp == nil {
		t.Fatal("response_path vector missing")
	}

	var key [256]byte
	copy(key[:], vectors.Hex(resp.AuthKey))
	authKey := crypto.NewAuthKey(key)
	payload := vectors.Hex(resp.Payload)

	// Server-style build with the reference padding bytes.
	got, err := mtproto.EncryptServerMessageData(authKey, payload, bytes.NewReader(vectors.Hex(resp.Padding)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, vectors.Hex(resp.Envelope)) {
		t.Fatalf("server envelope mismatch:\n want: %x\n  got: %x", vectors.Hex(resp.Envelope), got)
	}

	// The library's client-side decrypt must open it.
	plain, err := mtproto.DecryptMessageData(authKey, got)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(plain[:len(payload)], payload) {
		t.Fatal("round trip payload mismatch")
	}
}
