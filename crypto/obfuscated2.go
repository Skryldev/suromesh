package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
)

// Obfuscation keywords rejected from the header's first four bytes, matching
// the reference implementation (network/connection/tcpobfuscated.py).
var obfuscationKeywords = [4][]byte{
	[]byte("PVrG"), []byte("GET "), []byte("POST"), {0xEE, 0xEE, 0xEE, 0xEE},
}

// AbridgedTag is the obfuscation tag of the abridged packet codec.
var AbridgedTag = []byte{0xEF, 0xEF, 0xEF, 0xEF}

// ObfuscatedStream implements the "obfuscated2" AES-CTR layer used on top of
// the WebSocket transport: one continuous keystream in each direction,
// seeded by a 64-byte header sent in plaintext before any packet.
type ObfuscatedStream struct {
	enc, dec cipher.Stream
}

// InitObfuscated derives the stream from rand (which must yield at least 64
// bytes; os.Reader or a deterministic test source). It returns the 64-byte
// header that must be written before the first packet, plus the stream.
//
// Header algorithm (verified against websocket.WebSocketObfuscatedIO):
//  1. 64 random bytes, rejecting first byte 0xEF, the keywords above at
//     bytes 0..4, and zero bytes 4..8;
//  2. encrypt key = random[8:40], encrypt iv = random[40:56];
//  3. reversed = random[55:7:-1]; decrypt key = reversed[0:32], decrypt iv =
//     reversed[32:48];
//  4. tag (4 bytes) is placed at random[56:60], then random[56:64] is
//     replaced with encrypt(random)[56:64].
func InitObfuscated(rand io.Reader, tag []byte) ([]byte, *ObfuscatedStream, error) {
	if len(tag) != 4 {
		return nil, nil, errors.New("crypto: obfuscation tag must be 4 bytes")
	}
	var random [64]byte
	for {
		if _, err := io.ReadFull(rand, random[:]); err != nil {
			return nil, nil, err
		}
		if random[0] == 0xEF {
			continue
		}
		rejected := false
		for _, kw := range obfuscationKeywords {
			if string(random[:4]) == string(kw) {
				rejected = true
				break
			}
		}
		if rejected {
			continue
		}
		if string(random[4:8]) == "\x00\x00\x00\x00" {
			continue
		}
		break
	}

	// reversed = random[55:7:-1] in Python slice semantics.
	var reversed [48]byte
	for i := 0; i < 48; i++ {
		reversed[i] = random[55-i]
	}

	encBlock, err := aes.NewCipher(random[8:40])
	if err != nil {
		return nil, nil, err
	}
	encStream := cipher.NewCTR(encBlock, random[40:56])
	decBlock, err := aes.NewCipher(reversed[:32])
	if err != nil {
		return nil, nil, err
	}
	decStream := cipher.NewCTR(decBlock, reversed[32:48])

	header := make([]byte, 64)
	copy(header, random[:])
	copy(header[56:60], tag)
	// Splice the encrypted tail: encrypt all 64 bytes from keystream offset
	// 0 (matching encryptor.encrypt(bytes(random))), overwrite 56..64 with
	// ciphertext[56:64]. The encrypt stream is then positioned at keystream
	// offset 64, ready for the first packet byte.
	enc := make([]byte, 64)
	encStream.XORKeyStream(enc, header)
	copy(header[56:64], enc[56:64])

	return header, &ObfuscatedStream{enc: encStream, dec: decStream}, nil
}

// StreamsFromHeader derives the stream pair from a received 64-byte header —
// the peer's (server) view of a header the client generated. The returned
// stream's Decrypt opens client-originated frames and its Encrypt produces
// frames the client can open, with keystream positions already past the
// header. Used by test doubles of the Soroush server.
func StreamsFromHeader(header []byte) (*ObfuscatedStream, error) {
	if len(header) != 64 {
		return nil, errors.New("crypto: obfuscation header must be 64 bytes")
	}
	// Server decrypt = client encrypt keys (same keystream, same position).
	encBlock, err := aes.NewCipher(header[8:40]) // client encrypt key
	if err != nil {
		return nil, err
	}
	decryptForServer := cipher.NewCTR(encBlock, header[40:56])
	// The client's encrypt stream consumed 64 keystream bytes for the
	// header splice, so client packets begin at keystream offset 64.
	skip := make([]byte, 64)
	decryptForServer.XORKeyStream(skip, skip)
	// Server encrypt = client decrypt keys (reversed derivation).
	var reversed [48]byte
	for i := 0; i < 48; i++ {
		reversed[i] = header[55-i]
	}
	decBlock, err := aes.NewCipher(reversed[:32])
	if err != nil {
		return nil, err
	}
	encryptForServer := cipher.NewCTR(decBlock, reversed[32:48])
	return &ObfuscatedStream{enc: encryptForServer, dec: decryptForServer}, nil
}

// Encrypt XORs data with the outbound keystream (continuous across calls).
func (s *ObfuscatedStream) Encrypt(data []byte) []byte {
	out := make([]byte, len(data))
	s.enc.XORKeyStream(out, data)
	return out
}

// Decrypt XORs data with the inbound keystream (continuous across calls).
func (s *ObfuscatedStream) Decrypt(data []byte) []byte {
	out := make([]byte, len(data))
	s.dec.XORKeyStream(out, data)
	return out
}
