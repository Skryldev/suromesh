package client

// Phase 13 fuzz targets: hostile media metadata must never panic the
// classifier, size selection, location resolution, or grouping.

import (
	"testing"
	"time"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// fuzzClock for grouping tests.
type fuzzClock struct{ t0 time.Time }

func (c *fuzzClock) Now() time.Time { return c.t0 }

func timeUnixNow() time.Time { return time.Unix(1700000000, 0) }

// FuzzMediaClassification: any wire media constructor classifies without
// panicking; unknown families become UnknownMedia.
func FuzzMediaClassification(f *testing.F) {
	f.Add([]byte{0x8c, 0x71, 0x8e, 0x87})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		mm, ok := obj.(gen.MessageMediaClass)
		if !ok {
			return
		}
		m := classifyMedia(mm)
		if m == nil && mm != nil {
			t.Fatalf("non-nil media classified to nil: %T", mm)
		}
	})
}

// FuzzPhotoSizeSelection: arbitrary size vectors must select
// deterministically and never panic (empty, unordered, duplicate,
// extreme dimensions).
func FuzzPhotoSizeSelection(f *testing.F) {
	f.Add(int32(1), int32(2), int32(100))
	f.Fuzz(func(t *testing.T, w, h, size int32) {
		p := &Photo{Sizes: []PhotoSize{
			{Kind: PhotoSizeExact, Type: "x", W: w, H: h, ByteCount: int64(size)},
			{Kind: PhotoSizeProgressive, Type: "y", W: h, H: w,
				Progressive: []int32{size, size * 2}, ByteCount: int64(size) * 2},
			{Kind: PhotoSizeEmptyK},
		}}
		l, okL := p.Largest()
		s, okS := p.Smallest()
		b, okB := p.BestSize(int64(size))
		if !okL || !okS || !okB {
			return // all-empty is possible via the sizes present
		}
		if l.ByteCount < s.ByteCount {
			t.Fatalf("largest < smallest: %d < %d", l.ByteCount, s.ByteCount)
		}
		if b.ByteCount > l.ByteCount {
			t.Fatalf("best exceeds largest: %d > %d", b.ByteCount, l.ByteCount)
		}
	})
}

// FuzzMediaLocation: hostile media metadata must be rejected (not
// fabricated into wire locations).
func FuzzMediaLocation(f *testing.F) {
	f.Add(int64(1), int64(2), int64(0), int64(100))
	f.Fuzz(func(t *testing.T, id, hash, refLen, size int64) {
		if refLen > 1024 || refLen < 0 {
			refLen = 0
		}
		doc := &Document{ID: id, AccessHash: hash,
			FileReference: make([]byte, refLen), Size: size}
		loc, gotSize, inline, err := resolveMediaLocation(doc)
		if err != nil {
			return // rejection fine
		}
		if inline != nil {
			t.Fatal("full document resolved inline")
		}
		if gotSize != size {
			t.Fatalf("size mismatch: %d vs %d", gotSize, size)
		}
		if loc == nil {
			t.Fatal("nil location without error")
		}
	})
}

// FuzzStrippedPhoto: hostile stripped payloads splice without panicking.
func FuzzStrippedPhoto(f *testing.F) {
	f.Add([]byte{1, 64, 64})
	f.Add([]byte{0, 1})
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, payload []byte) {
		out := strippedPhotoToJPG(payload)
		if len(payload) >= 3 && payload[0] == 1 {
			if len(out) != 623+len(payload)-3+2 {
				t.Fatalf("output length %d for payload %d", len(out), len(payload))
			}
		} else if string(out) != string(payload) {
			t.Fatal("header-less payload modified")
		}
	})
}

// FuzzAlbumGrouping: arbitrary grouped_id/msg sequences keep the
// collector bounded.
func FuzzAlbumGrouping(f *testing.F) {
	f.Add(int32(1), int32(2), int32(3))
	f.Fuzz(func(t *testing.T, gid, id, count int32) {
		if count < 0 || count > 100 {
			count = 10
		}
		ac := NewAlbumCollector()
		ac.MaxPending = 4
		ac.MaxPerAlbum = 3
		clock := &fuzzClock{t0: timeUnixNow()}
		ac.Now = clock.Now
		for i := int32(0); i < count; i++ {
			ac.Feed(Message{ID: int64(id + i), PeerID: PeerID{PeerChat, 55},
				GroupedID: int64(gid), InAlbum: true})
		}
		if len(ac.pending) > 4 {
			t.Fatalf("pending exceeded bound: %d", len(ac.pending))
		}
	})
}
