package mtproto_test

import (
	"bytes"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
)

func TestClientEnvelopeSelfRoundTrip(t *testing.T) {
	key := [256]byte{}
	for i := range key {
		key[i] = byte(i * 7)
	}
	authKey := crypto.NewAuthKey(key)
	payload := []byte("salt|session|framed TL payload goes here")
	env, err := mtproto.EncryptMessageData(authKey, payload, mtproto.Rand)
	if err != nil {
		t.Fatal(err)
	}
	// Client envelopes are opened with client (x=0) key derivation; the
	// plaintext includes padding, so compare the payload prefix.
	plain := openClientEnvelope(t, authKey, env)
	if !bytes.Equal(plain[:len(payload)], payload) {
		t.Fatal("round trip mismatch")
	}
}

// openClientEnvelope decrypts a client->server envelope with x=0 keys,
// mirroring what the server does; the library only exposes the client's
// server-envelope decrypt path.
func openClientEnvelope(t *testing.T, authKey *crypto.AuthKey, env []byte) []byte {
	t.Helper()
	var mk [16]byte
	copy(mk[:], env[8:24])
	aesKey, aesIV := mtproto.CalcAESKeyIV(&authKey.Key, mk, true)
	plain, err := crypto.DecryptIGE(env[24:], aesKey[:], aesIV[:])
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

// FuzzEnvelopeDecrypt must never panic on hostile ciphertext; every failure
// is an error, never a crash.
func FuzzEnvelopeDecrypt(f *testing.F) {
	key := [256]byte{}
	for i := range key {
		key[i] = byte(i)
	}
	f.Add([]byte{})
	f.Add(make([]byte, 40))
	f.Add(make([]byte, 112))
	f.Fuzz(func(t *testing.T, env []byte) {
		if len(env) > 1<<16 {
			return
		}
		authKey := crypto.NewAuthKey(key)
		plain, err := mtproto.DecryptMessageData(authKey, env)
		if err != nil {
			return
		}
		if len(plain) < 16 {
			t.Fatal("decrypt returned payload shorter than the header")
		}
	})
}
