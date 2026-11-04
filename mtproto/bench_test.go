package mtproto_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
)

func benchAuthKey() *crypto.AuthKey {
	key := [256]byte{}
	for i := range key {
		key[i] = byte(i * 3)
	}
	return crypto.NewAuthKey(key)
}

func benchPayload() []byte {
	return bytes.Repeat([]byte{0x5A}, 1024)
}

func BenchmarkEnvelopeEncrypt1K(b *testing.B) {
	authKey := benchAuthKey()
	payload := benchPayload()
	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := mtproto.EncryptMessageData(authKey, payload, mtproto.Rand); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEnvelopeDecrypt1K(b *testing.B) {
	key := [256]byte{}
	for i := range key {
		key[i] = byte(i * 3)
	}
	authKey := crypto.NewAuthKey(key)

	// Server-style envelope (x=8 keys), the direction the library decrypts.
	payload := benchPayload()
	plain := append(append([]byte{}, payload...), make([]byte, 12+(16-(len(payload)+12)%16)%16)...)
	if _, err := mtproto.Rand.Read(plain[len(payload):]); err != nil {
		b.Fatal(err)
	}
	sum := sha256.Sum256(append(append([]byte{}, key[96:128]...), plain...))
	var mk [16]byte
	copy(mk[:], sum[8:24])
	aesKey, aesIV := mtproto.CalcAESKeyIV(&key, mk, false)
	ct, err := crypto.EncryptIGE(plain, aesKey[:], aesIV[:])
	if err != nil {
		b.Fatal(err)
	}
	env := make([]byte, 0, 24+len(ct))
	env = binary.LittleEndian.AppendUint64(env, authKey.KeyID)
	env = append(env, mk[:]...)
	env = append(env, ct...)

	b.SetBytes(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := mtproto.DecryptMessageData(authKey, env); err != nil {
			b.Fatal(err)
		}
	}
}
