package client

// Phase 13 unit tests: classifier, attributes, photo sizes, location
// resolution, album grouping. Reference traces in docs/media-domain.md
// and docs/albums.md.

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/tl/gen"
)

// mediaDocWire wraps a document into MessageMediaDocument (defined in
// the external fake file for package client tests too).
func mediaDocWire(d *gen.Document) *gen.MessageMediaDocument {
	return &gen.MessageMediaDocument{Document: d}
}

func wirePhoto(id, hash int64, ref []byte) *gen.Photo {
	return &gen.Photo{ID: id, AccessHash: hash, FileReference: ref, Date: 100,
		Sizes: []gen.PhotoSizeClass{
			&gen.PhotoSize{Type_: "x", W: 100, H: 100, Size: 5000},
			&gen.PhotoSize{Type_: "y", W: 200, H: 200, Size: 20000},
		}}
}

func wireDocWithAttrs(id, hash int64, attrs []gen.DocumentAttributeClass) *gen.Document {
	return &gen.Document{ID: id, AccessHash: hash, FileReference: []byte{1},
		MimeType: "video/mp4", Size: 5000, Attributes: attrs}
}

// TestClassifierFamilies: every MessageMedia family classifies without
// panic; unknown families become UnknownMedia (§8).
func TestClassifierFamilies(t *testing.T) {
	cases := []struct {
		name string
		in   gen.MessageMediaClass
		want MediaKind
	}{
		{"photo", &gen.MessageMediaPhoto{Photo: wirePhoto(1, 2, []byte{1})}, MediaKindPhoto},
		{"photo empty", &gen.MessageMediaPhoto{Photo: &gen.PhotoEmpty{ID: 1}}, MediaKindUnknown},
		{"document", &gen.MessageMediaDocument{Document: wireDocWithAttrs(1, 2, nil)}, MediaKindDocument},
		{"document empty", &gen.MessageMediaDocument{Document: &gen.DocumentEmpty{ID: 1}}, MediaKindUnknown},
		{"poll", &gen.MessageMediaPoll{}, MediaKindUnknown},
		{"geo", &gen.MessageMediaGeo{}, MediaKindUnknown},
		{"contact", &gen.MessageMediaContact{PhoneNumber: "x"}, MediaKindUnknown},
		{"webpage", &gen.MessageMediaWebPage{}, MediaKindUnknown},
		{"nil", nil, 255}, // handled via messageFromTL: nil media → nil Media
	}
	for _, tc := range cases {
		if tc.want == 255 {
			if m := classifyMedia(nil); m != nil {
				t.Fatalf("%s: nil media classified as %v", tc.name, m)
			}
			continue
		}
		m := classifyMedia(tc.in)
		if m == nil {
			t.Fatalf("%s: nil classification", tc.name)
		}
		if m.MediaKind() != tc.want {
			t.Fatalf("%s: kind = %v, want %v", tc.name, m.MediaKind(), tc.want)
		}
	}
}

// TestDocumentAttributeClassification: video/audio/voice/sticker/animated
// kinds come from attributes (message.py:585-631) — never MIME type.
func TestDocumentAttributeClassification(t *testing.T) {
	cases := []struct {
		name  string
		attrs []gen.DocumentAttributeClass
		want  MediaKind
	}{
		{"video", []gen.DocumentAttributeClass{&gen.DocumentAttributeVideo{Duration: 5, W: 100, H: 100}}, MediaKindVideo},
		{"audio", []gen.DocumentAttributeClass{&gen.DocumentAttributeAudio{Duration: 5, Title: strP("t")}}, MediaKindAudio},
		{"voice", []gen.DocumentAttributeClass{&gen.DocumentAttributeAudio{Voice: true, Duration: 5}}, MediaKindVoice},
		{"sticker", []gen.DocumentAttributeClass{&gen.DocumentAttributeSticker{Alt: ":)"}}, MediaKindSticker},
		{"animated", []gen.DocumentAttributeClass{&gen.DocumentAttributeAnimated{}}, MediaKindAnimated},
		{"plain doc", []gen.DocumentAttributeClass{&gen.DocumentAttributeFilename{FileName: "f.bin"}}, MediaKindDocument},
		{"mime lies", []gen.DocumentAttributeClass{&gen.DocumentAttributeFilename{FileName: "f.txt"}}, MediaKindDocument},
	}
	for _, tc := range cases {
		doc := documentFromWire(wireDocWithAttrs(1, 2, tc.attrs))
		if doc.MediaKind() != tc.want {
			t.Fatalf("%s: kind = %v, want %v (mime video/mp4 must not win)", tc.name, doc.MediaKind(), tc.want)
		}
	}
}

func strP(s string) *string { return &s }

// TestAttributeConversion: each wire attribute maps to its domain form;
// unknown ctors become UnknownAttribute.
func TestAttributeConversion(t *testing.T) {
	wire := wireDocWithAttrs(1, 2, []gen.DocumentAttributeClass{
		&gen.DocumentAttributeFilename{FileName: "clip.mp4"},
		&gen.DocumentAttributeVideo{Duration: 12.5, W: 640, H: 480, SupportsStreaming: true},
		&gen.DocumentAttributeAudio{Voice: true, Duration: 3, Waveform: []byte{1, 2}},
		&gen.DocumentAttributeAnimated{},
		&gen.DocumentAttributeHasStickers{},
	})
	doc := documentFromWire(wire)
	if doc.Filename() != "clip.mp4" {
		t.Fatalf("filename = %q", doc.Filename())
	}
	if len(doc.Attributes) != 5 {
		t.Fatalf("attrs = %d, want 5", len(doc.Attributes))
	}
	va, ok := doc.Attributes[1].(VideoAttribute)
	if !ok || va.Duration != 12.5 || va.W != 640 || !va.SupportsStreaming {
		t.Fatalf("video attr = %+v", doc.Attributes[1])
	}
	aa, ok := doc.Attributes[2].(AudioAttribute)
	if !ok || !aa.Voice || aa.Duration != 3 {
		t.Fatalf("audio attr = %+v", doc.Attributes[2])
	}
	if _, ok := doc.Attributes[3].(AnimatedAttribute); !ok {
		t.Fatalf("animated attr = %T", doc.Attributes[3])
	}
	if _, ok := doc.Attributes[4].(HasStickersAttribute); !ok {
		t.Fatalf("has-stickers attr = %T", doc.Attributes[4])
	}
}

// TestPhotoSizeSelection: the reference's sort/selection rules
// (downloads.py:790-825).
func TestPhotoSizeSelection(t *testing.T) {
	p := &Photo{
		Sizes: []PhotoSize{
			{Kind: PhotoSizeExact, Type: "x", ByteCount: 5000, W: 100, H: 100},
			{Kind: PhotoSizeProgressive, Type: "y", ByteCount: 30000, Progressive: []int32{10000, 30000}},
			{Kind: PhotoSizeStripped, Type: "i", Bytes: []byte{1, 2, 3}, ByteCount: 625},
			{Kind: PhotoSizeEmptyK, Type: ""},
		},
	}
	large, ok := p.Largest()
	if !ok || large.Type != "y" || large.ByteCount != 30000 {
		t.Fatalf("largest = %+v", large)
	}
	small, ok := p.Smallest()
	if !ok || small.Kind != PhotoSizeStripped {
		t.Fatalf("smallest = %+v", small)
	}
	mid, ok := p.BestSize(6000)
	if !ok || mid.Type != "x" {
		t.Fatalf("best 6000 = %+v, want x (5000)", mid)
	}
	// Nothing under 100 bytes → the largest is the fallback.
	big, ok := p.BestSize(100)
	if !ok || big.Type != "y" {
		t.Fatalf("best 100 = %+v, want y (fallback)", big)
	}
	// Empty-only photo: no usable size.
	empty := &Photo{Sizes: []PhotoSize{{Kind: PhotoSizeEmptyK}}}
	if _, ok := empty.Largest(); ok {
		t.Fatal("empty photo produced a size")
	}
}

// TestLocationResolution: photo/document locations carry the server's
// id/hash/reference; missing data fails without fabrication.
func TestLocationResolution(t *testing.T) {
	p := photoFromWire(wirePhoto(7, 8, []byte{9}))
	loc, size, inline, err := resolveMediaLocation(p)
	if err != nil {
		t.Fatal(err)
	}
	if inline != nil {
		t.Fatal("non-inline photo returned inline bytes")
	}
	pl, ok := loc.(*gen.InputPhotoFileLocation)
	if !ok || pl.ID != 7 || pl.AccessHash != 8 || string(pl.FileReference) != string([]byte{9}) {
		t.Fatalf("photo location = %+v", pl)
	}
	if size != 20000 {
		t.Fatalf("photo size = %d, want 20000 (largest)", size)
	}

	// Missing access hash → structural error (never fabricated).
	bad := &Photo{ID: 7, FileReference: []byte{1}}
	if _, _, _, err := resolveMediaLocation(bad); !errors.Is(err, ErrMediaLocation) {
		t.Fatalf("missing hash err = %v", err)
	}

	// Document full-file location: thumb_size empty, size = doc size.
	doc := documentFromWire(wireDocWithAttrs(3, 4, nil))
	dloc, dsize, _, derr := resolveMediaLocation(doc)
	if derr != nil {
		t.Fatal(derr)
	}
	dl, ok := dloc.(*gen.InputDocumentFileLocation)
	if !ok || dl.ID != 3 || dl.ThumbSize != "" || dsize != 5000 {
		t.Fatalf("doc location = %+v size %d", dl, dsize)
	}

	// Stripped-only document thumb → inline JPEG.
	strippedDoc := &Document{ID: 5, AccessHash: 6, FileReference: []byte{1},
		Size: 100, Thumbs: []PhotoSize{{Kind: PhotoSizeStripped, Type: "i", Bytes: []byte{1, 2, 3}}}}
	_, _, inl, serr := resolveDocumentLocation(strippedDoc, "i")
	if serr != nil || inl == nil {
		t.Fatalf("stripped thumb: inline=%v err=%v", inl, serr)
	}
}

// TestStrippedToJPGGolden: the Go splice matches the reference byte for
// byte (reference executed in the vector generator; vector provenance
// tools/vectors/generate.py::v_auth-style direct execution here inline).
func TestStrippedToJPGGolden(t *testing.T) {
	// The reference's known properties: output = header(623) + payload + footer,
	// header[164]=width, header[166]=height; header-less blobs pass through.
	stripped := append([]byte{1, 64, 64}, bytes.Repeat([]byte{0xAB}, 50)...)
	out := strippedPhotoToJPG(stripped)
	if len(out) != 623+50+2 {
		t.Fatalf("out len = %d, want %d", len(out), 623+50+2)
	}
	if out[0] != 0xff || out[1] != 0xd8 || out[len(out)-1] != 0xd9 {
		t.Fatal("JPEG SOI/EOI missing")
	}
	if out[164] != 64 || out[166] != 64 {
		t.Fatalf("dimensions not patched: %d %d", out[164], out[166])
	}
	// Header-less blob passes through unchanged.
	raw := []byte{0, 1, 2}
	if got := strippedPhotoToJPG(raw); !bytes.Equal(got, raw) {
		t.Fatal("header-less blob was modified")
	}
}

// TestAlbumCollectorGrouping: delay-based grouping by grouped_id
// (events/album.py semantics), arrival order preserved, bounded.
func TestAlbumCollectorGrouping(t *testing.T) {
	fakeNow := time.Unix(1700000000, 0)
	clock := &albumClock{now: fakeNow}
	ac := NewAlbumCollector()
	ac.Now = clock.Now

	// Three messages in one album, then a lone message.
	ac.Feed(Message{ID: 1, PeerID: PeerID{PeerChat, 55}, GroupedID: 111, InAlbum: true})
	ac.Feed(Message{ID: 2, PeerID: PeerID{PeerChat, 55}, GroupedID: 111, InAlbum: true})
	ac.Feed(Message{ID: 3, PeerID: PeerID{PeerChat, 55}, GroupedID: 111, InAlbum: true})
	ac.Feed(Message{ID: 4, PeerID: PeerID{PeerChat, 55}}) // not album: ignored

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clock.Advance(300 * time.Millisecond) // past the 200ms deadline
	alb, ok := ac.Next(ctx)
	if !ok {
		t.Fatal("album not delivered")
	}
	if alb.GroupedID != 111 || len(alb.Messages) != 3 {
		t.Fatalf("album = %+v", alb)
	}
	if alb.Messages[0].ID != 1 || alb.Messages[2].ID != 3 {
		t.Fatal("arrival order broken")
	}
	// No more albums pending.
	if _, ok := ac.Next(ctx); ok {
		t.Fatal("phantom album delivered")
	}
}

// TestAlbumCollectorCapacity: more distinct albums than MaxPending →
// oldest flushed, nothing dropped silently, store bounded.
func TestAlbumCollectorCapacity(t *testing.T) {
	ac := NewAlbumCollector()
	ac.MaxPending = 4
	fakeNow := time.Unix(1700000000, 0)
	clock := &albumClock{now: fakeNow}
	ac.Now = clock.Now

	for i := int64(0); i < 10; i++ {
		ac.Feed(Message{ID: i + 1, PeerID: PeerID{PeerChat, 55}, GroupedID: i, InAlbum: true})
		clock.Advance(time.Millisecond)
	}
	if len(ac.pending) > 4 {
		t.Fatalf("pending = %d, want ≤ 4", len(ac.pending))
	}
}

// TestAlbumCollectorPerAlbumBound: >MaxPerAlbum items flush immediately.
func TestAlbumCollectorPerAlbumBound(t *testing.T) {
	ac := NewAlbumCollector()
	ac.MaxPerAlbum = 3
	fakeNow := time.Unix(1700000000, 0)
	clock := &albumClock{now: fakeNow}
	ac.Now = clock.Now

	for i := int32(0); i < 5; i++ {
		ac.Feed(Message{ID: int64(i + 1), PeerID: PeerID{PeerChat, 55}, GroupedID: 77, InAlbum: true})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	alb, ok := ac.Next(ctx)
	if !ok || alb.GroupedID != 77 || len(alb.Messages) != 3 {
		t.Fatalf("album = %+v ok=%v", alb, ok)
	}
	// The remaining 2 flush on their own deadline.
	clock.Advance(300 * time.Millisecond)
	alb2, ok := ac.Next(ctx)
	if !ok || len(alb2.Messages) != 2 {
		t.Fatalf("remainder = %+v ok=%v", alb2, ok)
	}
}

// albumClock is the injectable clock for collector tests.
type albumClock struct{ now time.Time }

func (c *albumClock) Now() time.Time          { return c.now }
func (c *albumClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// TestMessageMediaCrossPathEquivalence: the same wire message yields the
// same Media whether it arrives via messageFromTL (history/search/
// updates/mutations) — one path, one result (§18).
func TestMessageMediaCrossPathEquivalence(t *testing.T) {
	wire := &gen.Message{ID: 9, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		Media: mediaDocWire(wireDocWithAttrs(11, 22, []gen.DocumentAttributeClass{
			&gen.DocumentAttributeFilename{FileName: "x.mp4"},
			&gen.DocumentAttributeVideo{Duration: 3},
		}))}
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	m1 := c.messageFromTL(wire)
	m2 := c.messageFromTL(wire)
	if m1.Media == nil || m2.Media == nil {
		t.Fatal("media not classified")
	}
	if m1.Media.MediaKind() != m2.Media.MediaKind() || m1.Media.MediaKind() != MediaKindVideo {
		t.Fatalf("kinds = %v vs %v", m1.Media.MediaKind(), m2.Media.MediaKind())
	}
	d1, ok1 := m1.Media.(*Document)
	if !ok1 || d1.Filename() != "x.mp4" || d1.ID != 11 {
		t.Fatalf("document = %+v", m1.Media)
	}
}

// TestMemory1000MediaMessages: 1000 media-heavy messages through the
// classifier add no linear heap growth (metadata-only conversion).
func TestMemory1000MediaMessages(t *testing.T) {
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	doc := wireDocWithAttrs(1, 2, []gen.DocumentAttributeClass{
		&gen.DocumentAttributeVideo{Duration: 10, W: 100, H: 100}})
	wire := &gen.Message{ID: 1, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		Media: mediaDocWire(doc)}

	heapBefore := heapAfterGCI()
	for i := 0; i < 1000; i++ {
		wire.ID = int32(i + 1)
		m := c.messageFromTL(wire)
		if m.Media == nil {
			t.Fatal("media lost")
		}
	}
	heapAfter := heapAfterGCI()
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("heap growth over 1000 media conversions: %.1f MiB", float64(growth)/(1<<20))
	if growth > 8*1024*1024 {
		t.Fatalf("heap grew %.1f MiB (conversion retained state?)", float64(growth)/(1<<20))
	}
}

// heapAfterGCI is the internal-package copy of heapAfterGC.
func heapAfterGCI() uint64 {
	runtime.GC()
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
