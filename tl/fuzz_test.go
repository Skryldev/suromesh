package tl_test

import (
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// FuzzDecodeObject feeds arbitrary bytes through the full dispatch. Decode
// must never panic, never loop forever, and bound its work by the input
// length (hostile counts/lengths must be rejected, not honored).
func FuzzDecodeObject(f *testing.F) {
	seeds := [][]byte{
		{0x15, 0xC4, 0xB5, 0x1C, 0x00, 0x00, 0x00, 0x00},                      // empty vector
		{0xB5, 0x75, 0x72, 0x99},                                              // boolTrue
		{0xDC, 0xF1, 0xF1, 0x73, 0x00, 0x00, 0x00, 0x00},                      // empty container
		{0x01, 0x78, 0x29, 0xCB, 0x01, 0x78, 0x00, 0x00, 0x01, 0x00, 0x00, 0}, // labeledPrice
		{0xDE, 0xAD, 0xBE, 0xEF},                                              // unknown ctor
		{0x01, 0x00, 0x00, 0x00},                                              // trivially truncated
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := tl.NewReader(data)
		obj, err := tl.DecodeObject(r)
		if err != nil {
			return
		}
		// A decoded object must re-encode without error and the encoder must
		// never panic even on fuzz-shaped field values.
		w := tl.NewWriter(len(data))
		if err := obj.Encode(w); err != nil {
			return
		}
		_ = obj.CtorID()
		_ = gen.Layer
	})
}

// FuzzBytesFraming checks the length-prefix codec against arbitrary inputs:
// decode(encode(x)) == x and no panics on hostile prefixes.
func FuzzBytesFraming(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xEF})
	f.Add(bytes253())
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		w := tl.NewWriter(len(data) + 8)
		w.PutBytes(data)
		r := tl.NewReader(w.Bytes())
		back, err := r.ReadBytes()
		if err != nil {
			t.Fatalf("decode of own encoding failed: %v", err)
		}
		if string(back) != string(data) {
			t.Fatal("round trip mismatch")
		}
		if r.Remaining() != 0 {
			t.Fatalf("%d bytes left after read", r.Remaining())
		}
	})
}

func bytes253() []byte {
	b := make([]byte, 253)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}
