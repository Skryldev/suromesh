package abridged_test

import (
	"bytes"
	"testing"

	"github.com/Skryldev/suromesh/transport/abridged"
)

func TestEncodeBoundaries(t *testing.T) {
	cases := []struct {
		n    int
		want []byte
	}{
		// len 0 → single 0 byte header
		{0, []byte{0x00}},
		// 4 bytes → 1 word → header 0x01
		{4, []byte{0x01}},
		// 504 bytes → 126 words → short form
		{504, []byte{126}},
		// 508 bytes → 127 words → long form 0x7F + 0x7F LE
		{508, []byte{0x7F, 0x7F, 0x00, 0x00}},
		// 65536 bytes → 16384 words → long form LE u24
		{65536, []byte{0x7F, 0x00, 0x40, 0x00}},
	}
	for _, c := range cases {
		payload := bytes.Repeat([]byte{0xAB}, c.n)
		got, err := abridged.Encode(nil, payload)
		if err != nil {
			t.Fatalf("n=%d: %v", c.n, err)
		}
		if !bytes.HasPrefix(got, c.want) {
			t.Fatalf("n=%d: header % x, want % x", c.n, got[:len(c.want)], c.want)
		}
		if len(got) != len(c.want)+c.n {
			t.Fatalf("n=%d: framed %d bytes, want %d", c.n, len(got), len(c.want)+c.n)
		}
	}
	// Payload not multiple of 4 is a hard error.
	if _, err := abridged.Encode(nil, []byte{1, 2, 3}); err == nil {
		t.Fatal("non-multiple-of-4 payload accepted")
	}
	// Beyond the 3-byte length space is a hard error.
	big := make([]byte, (0xFFFFFF+1)*4)
	if _, err := abridged.Encode(nil, big); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestFeedFragmented(t *testing.T) {
	// Three packets of different sizes, split at every possible boundary —
	// the decoder must reconstruct them from arbitrary fragmentation.
	p1 := bytes.Repeat([]byte{1}, 16)
	p2 := bytes.Repeat([]byte{2}, 508)
	p3 := bytes.Repeat([]byte{3}, 4096)
	var stream []byte
	for _, p := range [][]byte{p1, p2, p3} {
		s, err := abridged.Encode(nil, p)
		if err != nil {
			t.Fatal(err)
		}
		stream = append(stream, s...)
	}

	d := abridged.NewDecoder(0)
	var got [][]byte
	// One byte at a time — the worst possible fragmentation.
	for i := 0; i < len(stream); i++ {
		var err error
		got, err = d.Feed(stream[i:i+1], got)
		if err != nil {
			t.Fatalf("feed at %d: %v", i, err)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d packets, want 3", len(got))
	}
	if !bytes.Equal(got[0], p1) || !bytes.Equal(got[1], p2) || !bytes.Equal(got[2], p3) {
		t.Fatal("packet content mismatch")
	}
}

func TestFeedCoalescedAndZeroLength(t *testing.T) {
	// Several packets inside one chunk, including zero-length packets which
	// must be skipped (reference recv semantics).
	var stream []byte
	for i := 0; i < 5; i++ {
		s, _ := abridged.Encode(nil, nil) // zero-length
		stream = append(stream, s...)
		s2, _ := abridged.Encode(nil, []byte{1, 2, 3, 4})
		stream = append(stream, s2...)
	}
	d := abridged.NewDecoder(0)
	got, err := d.Feed(stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d packets, want 5 (zero-length skipped)", len(got))
	}
	for _, p := range got {
		if !bytes.Equal(p, []byte{1, 2, 3, 4}) {
			t.Fatal("content mismatch")
		}
	}
}

func TestHostileLengthPrefix(t *testing.T) {
	// 0x7F + 0xFFFFFF words = 64 MiB claim: must be rejected before any
	// large allocation, with only a few bytes fed.
	d := abridged.NewDecoder(16 << 20)
	_, err := d.Feed([]byte{0x7F, 0xFF, 0xFF, 0xFF}, nil)
	if err == nil {
		t.Fatal("oversized prefix accepted")
	}
	// Same with a tighter bound: 257 words = 1028 bytes > 1 KiB bound.
	d2 := abridged.NewDecoder(1024)
	_, err = d2.Feed([]byte{0x7F, 0x01, 0x01, 0x00}, nil)
	if err == nil {
		t.Fatal("oversized prefix accepted under small bound")
	}
}

func TestTruncatedPacketIsBuffered(t *testing.T) {
	p := bytes.Repeat([]byte{9}, 64)
	frame, _ := abridged.Encode(nil, p)
	d := abridged.NewDecoder(0)
	got, err := d.Feed(frame[:len(frame)-10], nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("partial feed produced %v / %v", got, err)
	}
	got, err = d.Feed(frame[len(frame)-10:], nil)
	if err != nil || len(got) != 1 || !bytes.Equal(got[0], p) {
		t.Fatalf("completion feed failed: %v %v", got, err)
	}
}

func FuzzAbridgedFeed(f *testing.F) {
	f.Add([]byte{0x01, 0, 0, 0, 0x7F, 5, 0, 0})
	f.Add(bytes.Repeat([]byte{0xFF, 0x7F, 0xFF, 0xFF}, 4))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		d := abridged.NewDecoder(16 << 20)
		packets, err := d.Feed(data, nil)
		if err != nil {
			return
		}
		// Every decoded packet must be within bounds and re-encode to a
		// prefix of the input framing.
		for _, p := range packets {
			if len(p) > d.MaxPacket() {
				t.Fatal("packet exceeded bound")
			}
			if len(p)%4 != 0 {
				t.Fatal("packet not multiple of 4")
			}
			frame, err := abridged.Encode(nil, p)
			if err != nil {
				t.Fatal(err)
			}
			d2 := abridged.NewDecoder(0)
			again, err := d2.Feed(frame, nil)
			if err != nil || len(again) != 1 || !bytes.Equal(again[0], p) {
				t.Fatal("re-decode mismatch")
			}
		}
	})
}
