// Package protocol asserts Go-only protocol invariants that do not need
// Python reference vectors: framing bounds, monotonic IDs, tamper
// resistance, and shutdown-safe decoder behavior.
package protocol_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func TestWriterStringFramingBoundaries(t *testing.T) {
	// For every length around the short/long boundary the framing must be
	// exactly: [len][data][pad] or [0xFE][len:3][data][pad].
	for _, n := range []int{0, 1, 2, 3, 252, 253, 254, 255, 256, 4096, 1 << 20} {
		data := bytes.Repeat([]byte{0xA5}, n)
		w := tl.NewWriter(n + 8)
		w.PutBytes(data)
		got := w.Bytes()
		var want int
		if n < 254 {
			// 1-byte length + data + pad-to-4 of (len+1)
			if pad := (n + 1) % 4; pad != 0 {
				want = 1 + n + 4 - pad
			} else {
				want = 1 + n
			}
		} else {
			pad := (4 - n%4) % 4
			want = 4 + n + pad
		}
		if len(got) != want {
			t.Fatalf("len %d: framed size %d, want %d", n, len(got), want)
		}
		r := tl.NewReader(got)
		back, err := r.ReadBytes()
		if err != nil || !bytes.Equal(back, data) || r.Remaining() != 0 {
			t.Fatalf("len %d: round trip failed (err=%v)", n, err)
		}
	}
}

func TestReaderRejectsOverlongVectorCounts(t *testing.T) {
	// A hostile element count must not pass the bounds check.
	raw := []byte{0x15, 0xC4, 0xB5, 0x1C, 0xFF, 0xFF, 0x00, 0x00} // vector, count 65535, no elements
	r := tl.NewReader(raw)
	if _, err := r.ReadVectorBoxedHeader(); err == nil {
		t.Fatal("accepted vector count exceeding buffer")
	}
}

func TestMessageContainerRejectsBadInnerLength(t *testing.T) {
	// Inner message claiming more bytes than the container holds.
	var w []byte
	w = append(w, 0xdc, 0xf1, 0xf1, 0x73) // msg_container
	w = append(w, 1, 0, 0, 0)             // count 1
	w = append(w, 1, 0, 0, 0, 0, 0, 0, 0) // msg_id
	w = append(w, 1, 0, 0, 0)             // seq_no
	w = append(w, 0xF0, 0x00, 0x00, 0x00) // length 240 > remaining
	if _, err := tl.DecodeObject(tl.NewReader(w)); err == nil {
		t.Fatal("accepted inner length exceeding buffer")
	}
}

func TestMessageIDGeneratorMonotonic(t *testing.T) {
	var g mtproto.MessageIDGenerator
	prev := int64(0)
	for i := 0; i < 10000; i++ {
		id := g.NewMessageID()
		if id <= prev {
			t.Fatalf("non-monotonic msg_id %d after %d", id, prev)
		}
		if id%4 != 0 {
			t.Fatalf("msg_id %d not congruent 0 mod 4", id)
		}
		prev = id
	}
}

func TestSeqNoGenerator(t *testing.T) {
	var s mtproto.SeqNoGenerator
	if got := s.Next(false); got != 0 {
		t.Fatalf("first control seq %d", got)
	}
	if got := s.Next(true); got != 1 {
		t.Fatalf("first content seq %d", got)
	}
	if got := s.Next(true); got != 3 {
		t.Fatalf("second content seq %d", got)
	}
	if got := s.Next(false); got != 4 {
		t.Fatalf("second control seq %d", got)
	}
}

func TestEnvelopeTamperResistance(t *testing.T) {
	key := [256]byte{}
	for i := range key {
		key[i] = byte(i)
	}
	authKey := crypto.NewAuthKey(key)

	// Build a server-style envelope (x=8 keys) with the same formulas the
	// reference server uses, then decrypt it through the library path.
	serverEnvelope := func(payload []byte) []byte {
		pad := 12 + (-(len(payload)+12)%16+16)%16
		plain := append(append([]byte{}, payload...), make([]byte, pad)...)
		if _, err := mtproto.Rand.Read(plain[len(payload):]); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(append(append([]byte{}, key[96:128]...), plain...))
		var mk [16]byte
		copy(mk[:], sum[8:24])
		aesKey, aesIV := mtproto.CalcAESKeyIV(&key, mk, false)
		ct, err := crypto.EncryptIGE(plain, aesKey[:], aesIV[:])
		if err != nil {
			t.Fatal(err)
		}
		out := make([]byte, 0, 24+len(ct))
		out = binary.LittleEndian.AppendUint64(out, authKey.KeyID)
		out = append(out, mk[:]...)
		return append(out, ct...)
	}

	payload := bytes.Repeat([]byte("x"), 64)
	env := serverEnvelope(payload)
	if _, err := mtproto.DecryptMessageData(authKey, env); err != nil {
		t.Fatalf("pristine envelope rejected: %v", err)
	}
	// Every single-byte flip anywhere in the envelope must be detected.
	for _, pos := range []int{0, 8, 17, 30, len(env) / 2, len(env) - 1} {
		bad := append([]byte{}, env...)
		bad[pos] ^= 0x08
		if _, err := mtproto.DecryptMessageData(authKey, bad); err == nil {
			t.Fatalf("tampered byte %d accepted", pos)
		}
	}
	// Wrong auth key must be rejected.
	other := key
	other[0] ^= 1
	if _, err := mtproto.DecryptMessageData(crypto.NewAuthKey(other), env); err == nil {
		t.Fatal("envelope accepted under wrong auth key")
	}
}

func TestEnvelopeRejectsShortBuffers(t *testing.T) {
	key := [256]byte{}
	authKey := crypto.NewAuthKey(key)
	for n := 0; n < 40; n++ {
		if _, err := mtproto.DecryptMessageData(authKey, bytes.Repeat([]byte{0}, n)); err == nil {
			t.Fatalf("accepted %d-byte envelope", n)
		}
	}
}

func TestGzipPackedRoundTrip(t *testing.T) {
	payload := bytes.Repeat([]byte("compress me please "), 100)
	obj, err := tl.Unmarshal(marshal(&tl.GzipPacked{Data: payload}))
	if err != nil {
		t.Fatal(err)
	}
	gz, ok := obj.(*tl.GzipPacked)
	if !ok {
		t.Fatalf("type %T", obj)
	}
	if !bytes.Equal(gz.Data, payload) {
		t.Fatal("decompressed payload mismatch")
	}
}

func TestGzipBombBounded(t *testing.T) {
	// 64 MiB of zeros compress small; decode must refuse past the limit.
	big := bytes.Repeat([]byte{0}, 64<<20)
	gz := tl.Gzip(big)
	if _, err := tl.GunzipLimit(gz, 1<<20); err == nil {
		t.Fatal("decompression bomb not bounded")
	}
}

func TestDecodeObjectStopsAtUnknownConstructor(t *testing.T) {
	raw := []byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4}
	_, err := tl.DecodeObject(tl.NewReader(raw))
	if err == nil {
		t.Fatal("unknown constructor accepted")
	}
	if !tl.IsUnknownConstructor(err) {
		t.Fatalf("wrong error type: %v", err)
	}
}

func TestContainerDecodeAcrossMessages(t *testing.T) {
	// A container of three ping requests; the inner objects must decode in
	// order with correct bounds (rpc_result-style over-reads must not leak
	// into the next message).
	var container tl.MessageContainer
	for i := 0; i < 3; i++ {
		container.Messages = append(container.Messages, tl.TLMessage{
			MsgID: int64(i + 1), SeqNo: int32(2*i + 1),
			Obj: &gen.PingRequest{PingID: int64(i)},
		})
	}
	raw, err := tl.Marshal(&container)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := tl.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := obj.(*tl.MessageContainer)
	if !ok {
		t.Fatalf("type %T", obj)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages %d", len(got.Messages))
	}
	for i, m := range got.Messages {
		ping, ok := m.Obj.(*gen.PingRequest)
		if !ok || ping.PingID != int64(i) {
			t.Fatalf("message %d: %v", i, m.Obj)
		}
		if m.MsgID != int64(i+1) || m.SeqNo != int32(2*i+1) {
			t.Fatalf("message %d framing: %+v", i, m)
		}
	}
}

func TestWriterDeferredError(t *testing.T) {
	w := tl.NewWriter(8)
	w.PutBytes(bytes.Repeat([]byte{0}, int(1<<24))) // > 0xFFFFFF cannot be framed
	if w.Err() == nil {
		t.Fatal("oversized write did not record an error")
	}
	if !strings.Contains(w.Err().Error(), "payload too large") {
		t.Fatalf("unexpected error: %v", w.Err())
	}
}

func marshal(o tl.Object) []byte {
	b, err := tl.Marshal(o)
	if err != nil {
		panic(err)
	}
	return b
}
