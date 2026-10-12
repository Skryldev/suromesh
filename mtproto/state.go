// Package mtproto implements the MTProto 2.0 message layer that sits between
// TL objects and the transport: message ID/sequence generation, the
// authenticated encryption envelope, and the plain (unencrypted) framing
// used during authentication.
//
// Wire reference: splusthon/network/mtprotostate.py and
// splusthon/network/mtprotoplainsender.py in the reference implementation.
package mtproto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Skryldev/suromesh/crypto"
)

// Envelope errors.
var (
	ErrInvalidAuthKeyID = errors.New("mtproto: server replied with an invalid auth key id")
	ErrInvalidMsgKey    = errors.New("mtproto: received msg_key does not match expected one")
	ErrInvalidSessionID = errors.New("mtproto: server replied with a wrong session id")
	ErrEvenMsgID        = errors.New("mtproto: server sent an even msg_id")
	ErrInvalidBuffer    = errors.New("mtproto: invalid encrypted buffer")
)

// Message freshness policy from mtprotostate.py.
const (
	MsgTooOldDelta = 300 // seconds; older messages are dropped
	MsgTooNewDelta = 30  // seconds; newer messages are dropped
)

// MessageHeader is the plaintext frame around the TL payload(s):
// salt:int64 | session_id:int64 | messages...
type MessageHeader struct {
	Salt      int64
	SessionID int64
}

// CalcMsgKey computes msg_key for MTProto 2.0: the middle 16 bytes of
// SHA256(auth_key[88:120] || plaintext) for the client-to-server direction
// (x=0). The server-to-client check uses auth_key[96:128].
func CalcMsgKey(authKey *[256]byte, plaintext []byte) [16]byte {
	var buf []byte
	buf = append(buf, authKey[88:120]...)
	buf = append(buf, plaintext...)
	sum := sha256.Sum256(buf)
	var mk [16]byte
	copy(mk[:], sum[8:24])
	return mk
}

// CalcAESKeyIV derives the IGE key and IV for a message key (mtprotostate
// _calc_key; x = 0 client, 8 server):
//
//	sha256a = SHA256(msg_key || auth_key[x:x+36])
//	sha256b = SHA256(auth_key[x+40:x+76] || msg_key)
//	aes_key = sha256a[0:8]  | sha256b[8:24] | sha256a[24:32]
//	aes_iv  = sha256b[0:8]  | sha256a[8:24] | sha256b[24:32]
func CalcAESKeyIV(authKey *[256]byte, msgKey [16]byte, client bool) (key, iv [32]byte) {
	x := 0
	if !client {
		x = 8
	}
	a := sha256.Sum256(append(append([]byte{}, msgKey[:]...), authKey[x:x+36]...))
	b := sha256.Sum256(append(append([]byte{}, authKey[x+40:x+76]...), msgKey[:]...))

	copy(key[0:8], a[0:8])
	copy(key[8:24], b[8:24])
	copy(key[24:32], a[24:32])

	copy(iv[0:8], b[0:8])
	copy(iv[8:24], a[8:24])
	copy(iv[24:32], b[24:32])
	return key, iv
}

// EncryptMessageData builds a complete MTProto 2.0 encrypted envelope around
// payload (which must already be framed as salt|session_id|messages):
//
//	auth_key_id:int64 | msg_key:16 | AES-256-IGE(plaintext + padding)
//
// Padding is 12 + (-(len+12) mod 16) random bytes, read from rnd (crypto/
// rand.Reader in production; a deterministic source in tests).
func EncryptMessageData(authKey *crypto.AuthKey, payload []byte, rnd io.Reader) ([]byte, error) {
	if !authKey.HasValue {
		return nil, errors.New("mtproto: no auth key")
	}
	padding := 12 + (-(len(payload)+12)%16+16)%16

	total := len(payload) + padding
	plain := make([]byte, total)
	copy(plain, payload)
	if _, err := io.ReadFull(rnd, plain[len(payload):]); err != nil {
		return nil, fmt.Errorf("mtproto: padding randomness: %w", err)
	}

	msgKey := CalcMsgKey(&authKey.Key, plain)
	aesKey, aesIV := CalcAESKeyIV(&authKey.Key, msgKey, true)
	cipherText, err := crypto.EncryptIGE(plain, aesKey[:], aesIV[:])
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, 8+16+len(cipherText))
	out = binary.LittleEndian.AppendUint64(out, authKey.KeyID)
	out = append(out, msgKey[:]...)
	out = append(out, cipherText...)
	return out, nil
}

// DecryptMessageData validates and decrypts an encrypted envelope, returning
// the plaintext payload (salt|session_id|messages).
func DecryptMessageData(authKey *crypto.AuthKey, body []byte) ([]byte, error) {
	if len(body) < 8+16+16 {
		return nil, ErrInvalidBuffer
	}
	keyID := binary.LittleEndian.Uint64(body[:8])
	if keyID != authKey.KeyID {
		return nil, ErrInvalidAuthKeyID
	}
	var msgKey [16]byte
	copy(msgKey[:], body[8:24])

	aesKey, aesIV := CalcAESKeyIV(&authKey.Key, msgKey, false)
	plain, err := crypto.DecryptIGE(body[24:], aesKey[:], aesIV[:])
	if err != nil {
		return nil, err
	}

	// Security guideline check: msg_key must equal the middle 16 bytes of
	// SHA256(auth_key[96:128] || plaintext).
	sum := sha256.Sum256(append(append([]byte{}, authKey.Key[96:128]...), plain...))
	var expect [16]byte
	copy(expect[:], sum[8:24])
	if msgKey != expect {
		return nil, ErrInvalidMsgKey
	}
	return plain, nil
}

// ParsePlainPayload splits a decrypted payload into its header and message
// bytes, verifying the session ID.
func ParsePlainPayload(authKey *crypto.AuthKey, payload []byte, expectSession int64) (MessageHeader, error) {
	if len(payload) < 16 {
		return MessageHeader{}, ErrInvalidBuffer
	}
	var h MessageHeader
	h.Salt = int64(binary.LittleEndian.Uint64(payload[:8]))
	h.SessionID = int64(binary.LittleEndian.Uint64(payload[8:16]))
	if h.SessionID != expectSession {
		return h, ErrInvalidSessionID
	}
	return h, nil
}

// PlainMessage is an unencrypted MTProto message (auth key exchange only).
// Wire: auth_key_id=0:int64 | msg_id:int64 | length:int32 | body.
func PlainMessage(msgID int64, body []byte) []byte {
	out := make([]byte, 0, 20+len(body))
	out = binary.LittleEndian.AppendUint64(out, 0)
	out = binary.LittleEndian.AppendUint64(out, uint64(msgID))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, body...)
	return out
}

// MessageIDGenerator produces monotonic MTProto message IDs:
// (unix_seconds << 32) | (nanoseconds << 2), guarded against going
// backwards or colliding (last+4) exactly like mtprotostate._get_new_msg_id.
type MessageIDGenerator struct {
	Last   int64
	Offset int64 // time offset applied to the local clock
}

// NewMessageID returns the next message ID.
func (g *MessageIDGenerator) NewMessageID() int64 {
	now := time.Now().Unix() + g.Offset
	nanos := time.Now().UnixNano() % 1e9
	msgID := (now << 32) | (nanos << 2)
	if g.Last >= msgID {
		msgID = g.Last + 4
	}
	g.Last = msgID
	return msgID
}

// SeqNoGenerator produces MTProto sequence numbers (mtprotostate
// _get_seq_no): content-related messages increment the counter and use
// odd numbers; control messages use even numbers.
type SeqNoGenerator struct {
	seq int32
}

// Next returns the next sequence number.
func (s *SeqNoGenerator) Next(contentRelated bool) int32 {
	if contentRelated {
		out := s.seq*2 + 1
		s.seq++
		return out
	}
	return s.seq * 2
}

// Pad must consume randomness from the same source as production; Rand is
// re-exported so callers do not need to import crypto/rand twice.
var Rand io.Reader = rand.Reader

// NewSessionID draws a fresh random session ID (mtprotostate.reset:
// struct.unpack('q', os.urandom(8))). Additive Phase 4 helper used when a
// replacement connection needs a new session.
func NewSessionID() (int64, error) {
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b[:])), nil
}
