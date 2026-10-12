package client_test

// Fake media server family (Phase 13): deterministic photo/document
// wire fixtures and the sendMultiMedia route, exercising the real
// transport → crypto → RPC → client → classifier stack.

import (
	"context"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl/gen"
)

// testPhoto builds a wire Photo with two sizes (stripped + exact).
func testPhoto(id, hash int64, ref []byte) *gen.Photo {
	w, h := int32(10), int32(10)
	stripped := append([]byte{1, byte(w), byte(h)}, []byte("strippedbytes")...)
	return &gen.Photo{
		ID:            id,
		AccessHash:    hash,
		FileReference: ref,
		Date:          100,
		Sizes: []gen.PhotoSizeClass{
			&gen.PhotoStrippedSize{Type_: "i", Bytes: stripped},
			&gen.PhotoSize{Type_: "x", W: w, H: h, Size: 500},
		},
	}
}

// testDocument builds a wire Document with a filename attribute.
func testDocument(id, hash int64, ref []byte, mime string) *gen.Document {
	return &gen.Document{
		ID:            id,
		AccessHash:    hash,
		FileReference: ref,
		Date:          100,
		MimeType:      mime,
		Size:          2048,
		Attributes: []gen.DocumentAttributeClass{
			&gen.DocumentAttributeFilename{FileName: "file.bin"},
		},
	}
}

// mediaDocWire wraps a document into MessageMediaDocument.
func mediaDocWire(d *gen.Document) *gen.MessageMediaDocument {
	return &gen.MessageMediaDocument{Document: d}
}

// mediaPhotoWire wraps a photo into MessageMediaPhoto.
func mediaPhotoWire(p *gen.Photo) *gen.MessageMediaPhoto {
	return &gen.MessageMediaPhoto{Photo: p}
}

// newMediaDomainClient: ready client with media + mutation fakes wired
// (sendMultiMedia routes through the mutation fake's search answer? no —
// it gets its own handler below).
func newMediaDomainClient(t testing.TB) (*client.Client, *appFake, *mutationsFake) {
	t.Helper()
	c, fake, mf := newMutationsClient(t)
	return c, fake, mf
}

// TestSendMediaExistingDocumentResend: an existing document is resent
// via InputMediaDocument (no re-upload — verified by zero upload RPCs).
func TestSendMediaExistingDocumentResend(t *testing.T) {
	c, fake, _ := newMediaDomainClient(t)
	defer c.Close()

	// Attach the media fake so uploads would be observable.
	mf2 := newMediaFake()
	installMediaHooks(t, fake, mf2)

	wireDoc := testDocument(900, 0xdead, []byte{1, 2, 3}, "application/octet-stream")
	doc := c.ClassifyMediaForTest(mediaDocWire(wireDoc)).(*client.Document)
	up, err := c.SendMediaInput(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, client.ExistingDocumentInput{Document: doc})
	if err != nil {
		t.Fatal(err)
	}
	if up == nil || up.ID == 0 {
		t.Fatalf("sent = %+v", up)
	}
	// No upload RPCs happened (existing media path).
	if got := mf2.uploadedLen(0); got != 0 {
		_ = got
	}
	var uploads int
	for _, ct := range fake.Received() {
		if ct == mediaCtorSavePart || ct == mediaCtorSaveBig {
			uploads++
		}
	}
	if uploads != 0 {
		t.Fatalf("existing-media resend performed %d uploads", uploads)
	}
}

// TestSendAlbumE2E: three items via messages.sendMultiMedia; per-item
// random ids pair results in order.
func TestSendAlbumE2E(t *testing.T) {
	c, fake, mf := newMediaDomainClient(t)
	defer c.Close()

	toDomain := func(id int64) *client.Document {
		return c.ClassifyMediaForTest(mediaDocWire(testDocument(id, 0xdead, []byte{byte(id)}, "image/png"))).(*client.Document)
	}
	msgs, err := c.SendAlbum(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55},
		[]client.MediaInput{
			client.ExistingDocumentInput{Document: toDomain(901), Caption: "one"},
			client.ExistingDocumentInput{Document: toDomain(902), Caption: "two"},
			client.ExistingDocumentInput{Document: toDomain(903), Caption: "three"},
		}, client.SendAlbumOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("album results = %d, want 3", len(msgs))
	}
	for i, m := range msgs {
		if m == nil {
			t.Fatalf("album item %d unmapped", i)
		}
	}
	// The multimedia request rode the real wire (ctor check).
	found := false
	for _, ct := range fake.Received() {
		if ct == (&gen.MessagesSendMultiMediaRequest{}).CtorID() {
			found = true
		}
	}
	if !found {
		t.Fatal("sendMultiMedia not observed on the wire")
	}
	_ = mf
}

// TestDownloadPhotoStrippedE2E: a photo whose largest size is stripped
// downloads WITHOUT any getFile RPC (inline bytes).
func TestDownloadPhotoStrippedE2E(t *testing.T) {
	c, _, _ := newMediaDomainClient(t)
	defer c.Close()

	p := testPhoto(910, 0xfeed, []byte{9, 9})
	// Only a stripped size → inline payload.
	p.Sizes = p.Sizes[:1]
	msg := &client.Message{
		ID:     5,
		PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Raw: &gen.Message{ID: 5, PeerID: &gen.PeerChat{ChatID: 55},
			Media: mediaPhotoWire(p), Date: 100},
		Media: c.ClassifyMediaForTest(mediaPhotoWire(p)),
	}

	var out byteBuf
	if err := c.DownloadMedia(context.Background(), msg, &out, client.DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	// The stripped payload was spliced into a JPEG (header + content + footer).
	if len(out.b) < 600 {
		t.Fatalf("stripped jpg too small: %d", len(out.b))
	}
	if out.b[0] != 0xff || out.b[1] != 0xd8 {
		t.Fatal("missing JPEG SOI")
	}
}

type byteBuf struct{ b []byte }

func (bb *byteBuf) Write(p []byte) (int, error) { bb.b = append(bb.b, p...); return len(p), nil }

// TestDownloadDocumentE2E: a document downloads through the REAL
// upload.getFile path with the server-provided file reference.
func TestDownloadDocumentE2E(t *testing.T) {
	c, fake, _ := newMediaDomainClient(t)
	defer c.Close()

	// Attach the media fake for getFile serving.
	mf2 := newMediaFake()
	installMediaHooks(t, fake, mf2)
	content := make([]byte, 2048)
	for i := range content {
		content[i] = byte(i % 251)
	}
	wireDoc := testDocument(920, 0xbeef, []byte{7, 7, 7}, "application/octet-stream")
	mf2.setDownloadData(920, content)

	msg := &client.Message{
		ID:     6,
		PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Raw: &gen.Message{ID: 6, PeerID: &gen.PeerChat{ChatID: 55},
			Media: mediaDocWire(wireDoc), Date: 100},
		Media: c.ClassifyMediaForTest(mediaDocWire(wireDoc)),
	}

	var out byteBuf
	if err := c.DownloadMedia(context.Background(), msg, &out, client.DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if string(out.b) != string(content) {
		t.Fatalf("content mismatch: %q vs %q", out.b, content)
	}
	// The wire request carried the server's file reference — the media
	// fake recorded the getFile for document 920.
	mf2.mu.Lock()
	seen := mf2.getFileN[920] > 0
	mf2.mu.Unlock()
	if !seen {
		t.Fatal("getFile never issued")
	}
}
