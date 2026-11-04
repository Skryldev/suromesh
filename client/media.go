package client

// Media plumbing (Phase 8 origin, Phase 13 upgrade): DownloadMedia and
// the text-file SendMedia convenience. The domain model and the
// canonical classifier live in media_domain.go; the location resolver in
// media_location.go; the send layer in media_send.go.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl/gen"
)

// ErrUnsupportedMedia reports a message without modelable media
// (Phase 8 sentinel, retained).
var ErrUnsupportedMedia = errors.New("media: unsupported media type")

// mediaIdentity is the wire addressing identity (kind + id + access
// hash) of a location — file_reference excluded (rotatable).
type mediaIdentity struct {
	kind string
	id   int64
	hash int64
}

var mediaIdentityZero = mediaIdentity{}

func mediaIdentityOf(l gen.InputFileLocationClass) mediaIdentity {
	switch v := l.(type) {
	case *gen.InputDocumentFileLocation:
		return mediaIdentity{"doc", v.ID, v.AccessHash}
	case *gen.InputPhotoFileLocation:
		return mediaIdentity{"photo", v.ID, v.AccessHash}
	}
	return mediaIdentityZero
}

// DownloadMedia extracts the media from a Message and streams it to w.
// The canonical path (Phase 13): Message.Media (classifier) →
// resolveMediaLocation → Phase 8 transfer engine. Photos and documents
// are supported; unsupported families fail with ErrUnsupportedMedia.
//
// File-reference refresh (reference downloads.py:120-148): on
// FILE_REFERENCE_EXPIRED the message is re-fetched by ID, the fresh
// server-provided file reference is adopted (only when the media id is
// unchanged), and the download is retried exactly once.
func (c *Client) DownloadMedia(ctx context.Context, msg *Message, w io.Writer, opts DownloadOptions) error {
	return c.downloadMediaOnce(ctx, msg, w, opts, true)
}

func (c *Client) downloadMediaOnce(ctx context.Context, msg *Message, w io.Writer, opts DownloadOptions, allowRefresh bool) error {
	if msg == nil || msg.Raw == nil {
		return errors.New("media: nil message")
	}
	if msg.Media == nil {
		return ErrUnsupportedMedia
	}
	loc, size, inline, lerr := resolveMediaLocation(msg.Media)
	if lerr != nil {
		return fmt.Errorf("%w: %v", ErrUnsupportedMedia, lerr)
	}
	// Inline payloads (cached/stripped sizes): no RPC at all — the bytes
	// ARE the content (reference _download_cached_photo_size,
	// downloads.py:826-845).
	if inline != nil {
		_, werr := w.Write(inline)
		return werr
	}
	dopts := opts
	if dopts.Size == 0 && size > 0 {
		dopts.Size = size
	}
	err := c.Download(ctx, loc, w, dopts)
	if err == nil || !allowRefresh || !isFileReferenceExpired(err) {
		return err
	}
	// Reference refresh path: re-fetch the message, adopt the new file
	// reference when the media id is unchanged, retry once.
	if _, ferr := c.refreshMediaReference(ctx, msg, loc); ferr != nil {
		return err // surface the original failure
	}
	return c.downloadMediaOnce(ctx, msg, w, opts, false)
}

// isFileReferenceExpired matches the server's FILE_REFERENCE_EXPIRED.
func isFileReferenceExpired(err error) bool {
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Type == "FILE_REFERENCE_EXPIRED"
}

// refreshMediaReference re-fetches the message by ID and rotates the
// in-place Media file reference when the server provides a newer one
// (downloads.py:125-147: the media id must be unchanged; a different
// media means the content was edited — the original error stands).
func (c *Client) refreshMediaReference(ctx context.Context, msg *Message, origLoc gen.InputFileLocationClass) (*Message, error) {
	if msg.PeerID.Kind == 0 || msg.ID <= 0 {
		return nil, errors.New("media: cannot refresh without peer and message id")
	}
	fetched, err := c.GetMessages(ctx, msg.PeerID, MessageQuery{Limit: 1, OffsetID: msg.ID + 1})
	if err != nil {
		return nil, err
	}
	if len(fetched) == 0 || fetched[0].ID != msg.ID || fetched[0].Media == nil {
		return nil, errors.New("media: refreshed message unavailable")
	}
	newLoc, _, _, lerr := resolveMediaLocation(fetched[0].Media)
	if lerr != nil {
		return nil, lerr
	}
	// Rotate in place: both locations must address the same media id.
	if sameMediaIdentity(origLoc, newLoc) {
		msg.Media = fetched[0].Media
		return fetched[0], nil
	}
	return nil, errors.New("media: media changed during reference refresh")
}

// sameMediaIdentity compares the wire addressing identity (kind + id +
// access hash) of two locations — file_reference intentionally excluded
// (it is the thing being rotated).
func sameMediaIdentity(a, b gen.InputFileLocationClass) bool {
	av, bv := mediaIdentityOf(a), mediaIdentityOf(b)
	return av != mediaIdentityZero && av == bv
}
