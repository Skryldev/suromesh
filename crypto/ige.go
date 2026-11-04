// Package crypto implements the Soroush Plus transport cryptography:
// AES-256-IGE for MTProto message payloads, the obfuscated2 AES-CTR stream
// for the WebSocket transport, and AuthKey identity derivation.
//
// Only protocol-required framing is implemented here; the block cipher
// itself, CTR mode, SHA-1 and SHA-256 all come from the Go standard library.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
)

// ErrIGEBlockSize is returned for payloads that are not a multiple of the
// AES block size.
var ErrIGEBlockSize = errors.New("crypto: ige payload must be a multiple of 16 bytes")

// EncryptIGE encrypts plaintext with AES-256-IGE.
//
// key is 32 bytes; iv is 32 bytes where iv[:16] is the chaining ciphertext
// block and iv[16:] is the chaining plaintext block:
//
//	c_i = E(p_i xor c_{i-1}) xor p_{i-1}
//
// The returned slice is freshly allocated; the inputs are not modified.
func EncryptIGE(plain, key, iv []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, aes.KeySizeError(len(key))
	}
	if len(iv) != 32 {
		return nil, errors.New("crypto: ige iv must be 32 bytes")
	}
	if len(plain)%16 != 0 {
		return nil, ErrIGEBlockSize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(plain))
	igeCrypt(block, plain, out, iv[:16], iv[16:], true)
	return out, nil
}

// DecryptIGE decrypts AES-256-IGE ciphertext:
//
//	p_i = D(c_i xor p_{i-1}) xor c_{i-1}
func DecryptIGE(cipherText, key, iv []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, aes.KeySizeError(len(key))
	}
	if len(iv) != 32 {
		return nil, errors.New("crypto: ige iv must be 32 bytes")
	}
	if len(cipherText)%16 != 0 {
		return nil, ErrIGEBlockSize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(cipherText))
	igeCrypt(block, cipherText, out, iv[:16], iv[16:], false)
	return out, nil
}

// igeCrypt runs the IGE chain over full blocks. enc selects the block
// direction; x is the input, y the output.
//
// Encrypt chains: c_i = E(p_i xor c_{i-1}) xor p_{i-1}
// Decrypt mirrors:  p_i = D(c_i xor p_{i-1}) xor c_{i-1}
func igeCrypt(block cipher.Block, x, y, ivC, ivP []byte, enc bool) {
	var prevC, prevP [16]byte
	copy(prevC[:], ivC)
	copy(prevP[:], ivP)

	var in, out [16]byte
	for off := 0; off < len(x); off += 16 {
		if enc {
			for i := 0; i < 16; i++ {
				in[i] = x[off+i] ^ prevC[i]
			}
			block.Encrypt(out[:], in[:])
			for i := 0; i < 16; i++ {
				y[off+i] = out[i] ^ prevP[i]
			}
			copy(prevC[:], y[off:off+16])
			copy(prevP[:], x[off:off+16])
		} else {
			for i := 0; i < 16; i++ {
				in[i] = x[off+i] ^ prevP[i]
			}
			block.Decrypt(out[:], in[:])
			for i := 0; i < 16; i++ {
				y[off+i] = out[i] ^ prevC[i]
			}
			copy(prevC[:], x[off:off+16])
			copy(prevP[:], y[off:off+16])
		}
	}
}
