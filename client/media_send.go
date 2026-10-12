package client

// Media sending (Phase 13): domain-level media inputs, existing-media
// resend without re-upload, and album sends via messages.sendMultiMedia.
//
// Reference:
//	send_file → upload → InputMediaUploaded*   client/uploads.py (Phase 8)
//	existing media → InputMediaPhoto/Document  uploads.py:866-890 + 866-890
//	album → messages.sendMultiMedia            messages.py (send_file's
//	                                           multi-file branch)
//	InputSingleMedia{media, random_id, message, entities}
//
// Distinction preserved (§13): Upload → InputMediaUploaded* (new bytes);
// existing server media → InputMediaPhoto/InputMediaDocument with the
// server-provided id+access_hash+file_reference (never re-uploaded).

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Skryldev/suromesh/tl/gen"
)

func errorsNew(msg string) error { return errors.New(msg) }

// MediaInput is one item of a media send: either freshly uploaded bytes
// or a reference to existing server media.
type MediaInput interface{ mediaInput() }

// UploadedPhotoInput sends freshly uploaded bytes as a photo.
type UploadedPhotoInput struct {
	Upload  *UploadedFile
	Caption string
}

func (UploadedPhotoInput) mediaInput() {}

// UploadedDocumentInput sends freshly uploaded bytes as a document (or
// as the media the attributes imply).
type UploadedDocumentInput struct {
	Upload   *UploadedFile
	MimeType string
	Caption  string
}

func (UploadedDocumentInput) mediaInput() {}

// ExistingPhotoInput resends a server-provided photo WITHOUT re-uploading
// (InputMediaPhoto with the server's id/hash/reference).
type ExistingPhotoInput struct {
	Photo   *Photo
	Caption string
}

func (ExistingPhotoInput) mediaInput() {}

// ExistingDocumentInput resends a server-provided document WITHOUT
// re-uploading (InputMediaDocument).
type ExistingDocumentInput struct {
	Document *Document
	Caption  string
}

func (ExistingDocumentInput) mediaInput() {}

// mediaInputToWire converts one MediaInput to the wire InputMedia form.
func (c *Client) mediaInputToWire(in MediaInput) (gen.InputMediaClass, error) {
	switch v := in.(type) {
	case UploadedPhotoInput:
		if v.Upload == nil {
			return nil, ErrInvalidMedia
		}
		return &gen.InputMediaUploadedPhoto{
			File:       v.Upload.AsInputFile(),
			TTLSeconds: nil,
		}, nil
	case UploadedDocumentInput:
		if v.Upload == nil {
			return nil, ErrInvalidMedia
		}
		mime := v.MimeType
		if mime == "" {
			mime = "application/octet-stream"
		}
		attrs := []gen.DocumentAttributeClass{
			&gen.DocumentAttributeFilename{FileName: v.Upload.Name},
		}
		return &gen.InputMediaUploadedDocument{
			File:       v.Upload.AsInputFile(),
			MimeType:   mime,
			Attributes: attrs,
			ForceFile:  true,
		}, nil
	case ExistingPhotoInput:
		if v.Photo == nil {
			return nil, ErrInvalidMedia
		}
		// Server-provided addressing only — id/hash/reference verbatim.
		return &gen.InputMediaPhoto{
			ID: &gen.InputPhoto{
				ID:            v.Photo.ID,
				AccessHash:    v.Photo.AccessHash,
				FileReference: v.Photo.FileReference,
			},
		}, nil
	case ExistingDocumentInput:
		if v.Document == nil {
			return nil, ErrInvalidMedia
		}
		return &gen.InputMediaDocument{
			ID: &gen.InputDocument{
				ID:            v.Document.ID,
				AccessHash:    v.Document.AccessHash,
				FileReference: v.Document.FileReference,
			},
		}, nil
	default:
		return nil, fmt.Errorf("%w: %T", ErrInvalidMedia, in)
	}
}

// SendMedia uploads a file from a reader and sends it as a document
// (Phase 8 API, retained: implemented on top of SendMediaInput).
func (c *Client) SendMedia(ctx context.Context, peer Peer, r io.Reader, opts UploadOptions) (*Message, error) {
	uploaded, err := c.Upload(ctx, r, opts)
	if err != nil {
		return nil, err
	}
	return c.SendMediaInput(ctx, peer, UploadedDocumentInput{
		Upload:   uploaded,
		MimeType: "application/octet-stream",
	})
}

// SendMediaInput sends one media item (upload or existing-media resend)
// and returns the sent message (Phase 8 SendMedia's typed generalization;
// the old io.Reader form remains and delegates here).
func (c *Client) SendMediaInput(ctx context.Context, peer PeerRef, in MediaInput) (*Message, error) {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return nil, err
	}
	media, err := c.mediaInputToWire(in)
	if err != nil {
		return nil, err
	}
	caption := captionOf(in)
	var rid [8]byte
	if _, err := randRead(rid[:]); err != nil {
		return nil, fmt.Errorf("media: random_id: %w", err)
	}
	req := &gen.MessagesSendMediaRequest{
		Peer:     input,
		Media:    media,
		Message:  caption,
		RandomID: int64BE(rid),
	}
	res, err := c.Invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	msgs, err := c.responseMessageList(ref, []int64{req.RandomID}, 0, res)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m != nil {
			return m, nil
		}
	}
	// Fall back to the Phase 6 update-family extraction when the server
	// answered without a random-id pairing.
	sm, err := c.messageFromUpdates(res)
	if err != nil {
		return nil, err
	}
	return sm.Message, nil
}

func captionOf(in MediaInput) string {
	switch v := in.(type) {
	case UploadedPhotoInput:
		return v.Caption
	case UploadedDocumentInput:
		return v.Caption
	case ExistingPhotoInput:
		return v.Caption
	case ExistingDocumentInput:
		return v.Caption
	}
	return ""
}

// SendAlbumOptions carries the album-level flags (sendMultiMedia flags).
type SendAlbumOptions struct {
	Silent     bool
	ClearDraft bool
	// ReplyTo replies the whole album to one message.
	ReplyTo int64
}

// SendAlbum sends multiple media items as one grouped album via
// messages.sendMultiMedia (reference send_file's multi branch). Per-item
// captions ride InputSingleMedia; per-item crypto random ids pair the
// results. Returns one message per input, in order, with nil holes for
// unmapped entries (messageparse.py:212-231 semantics).
func (c *Client) SendAlbum(ctx context.Context, peer PeerRef, items []MediaInput, opts SendAlbumOptions) ([]*Message, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if len(items) > albumMaxItems {
		return nil, fmt.Errorf("%w: %d items (max %d)", ErrInvalidMedia, len(items), albumMaxItems)
	}
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return nil, err
	}

	multi := make([]gen.InputSingleMediaClass, 0, len(items))
	randomIDs := make([]int64, 0, len(items))
	for _, in := range items {
		media, err := c.mediaInputToWire(in)
		if err != nil {
			return nil, err
		}
		var rid [8]byte
		if _, err := randRead(rid[:]); err != nil {
			return nil, fmt.Errorf("media: random_id: %w", err)
		}
		sm := &gen.InputSingleMedia{
			Media:    media,
			RandomID: int64BE(rid),
			Message:  captionOf(in),
		}
		multi = append(multi, sm)
		randomIDs = append(randomIDs, sm.RandomID)
	}

	var replyTo gen.InputReplyToClass
	if opts.ReplyTo > 0 {
		replyTo = &gen.InputReplyToMessage{ReplyToMsgID: int32(opts.ReplyTo)}
	}
	req := &gen.MessagesSendMultiMediaRequest{
		Peer:       input,
		ReplyTo:    replyTo,
		MultiMedia: multi,
		Silent:     opts.Silent,
		ClearDraft: opts.ClearDraft,
	}
	res, err := c.Invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	return c.responseMessageList(ref, randomIDs, 0, res)
}

// albumMaxItems: the wire's practical album bound (official clients send
// 10 per album; the schema has no hard limit — a defensive bound keeps
// requests and memory bounded).
const albumMaxItems = 10

// ErrInvalidMedia reports a malformed media send input.
var ErrInvalidMedia = errorsNew("media: invalid media input")
