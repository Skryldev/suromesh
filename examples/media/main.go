// Example: media upload and download through the real transfer engine.
//
// The bytes are uploaded part-by-part to the demo fake, sent as a document,
// and downloaded back — the round trip is verified byte-for-byte. Memory
// stays O(part size): streams, never whole-file buffers (docs/transfer.md).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/examples/internal/demo"
)

func main() {
	srv, err := demo.Start()
	if err != nil {
		demo.Fail("start", err)
	}
	defer srv.Close()
	demo.InstallAuth(srv, "12345")
	demo.InstallMessaging(srv)
	demo.InstallMedia(srv)

	c, err := srv.NewClient()
	if err != nil {
		demo.Fail("connect", err)
	}
	defer c.Close()
	ctx := context.Background()

	// 256 KiB of random content (2 upload parts at the default part size).
	content := make([]byte, 256*1024)
	if _, err := rand.Read(content); err != nil {
		demo.Fail("random", err)
	}

	// Upload — bounded memory, real saveFilePart RPCs.
	up, err := c.Upload(ctx, bytes.NewReader(content),
		client.UploadOptions{Size: int64(len(content)), FileName: "example.bin"})
	if err != nil {
		demo.Fail("upload", err)
	}
	fmt.Printf("uploaded: parts=%d big=%v md5=%v\n",
		up.Parts, up.IsBig, up.MD5Sum != "")

	// Send as a document (the fake echoes a Message with media).
	sent, err := c.SendMediaInput(ctx, client.PeerID{Kind: client.PeerChat, ID: 55},
		client.UploadedDocumentInput{Upload: up, MimeType: "application/octet-stream"})
	if err != nil {
		demo.Fail("sendMedia", err)
	}
	fmt.Printf("sent: message id=%d\n", sent.ID)

	// Download the same document back and verify the bytes.
	rcv := &client.Message{
		ID:     sent.ID,
		PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Raw:    sent.Raw,
		Media:  sent.Media,
	}
	var out bytes.Buffer
	if err := c.DownloadMedia(ctx, rcv, &out, client.DownloadOptions{}); err != nil {
		demo.Fail("download", err)
	}
	if !bytes.Equal(out.Bytes(), content) {
		demo.Fail("round trip", fmt.Errorf("downloaded %d bytes, want %d equal bytes",
			out.Len(), len(content)))
	}
	fmt.Printf("downloaded: %d bytes, round trip verified\n", out.Len())

}
