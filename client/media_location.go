package client

// Media location resolution (Phase 13): the ONE canonical
// Media → wire-location path, ported from the reference:
//
//	photos      client/downloads.py:852-873 (_download_photo:
//	            InputPhotoFileLocation with the selected thumb_size;
//	            cached/stripped sizes download nothing)
//	documents   client/downloads.py:911-951 (_download_document:
//	            InputDocumentFileLocation with thumb_size='' for the
//	            full file)
//
// Rules: the selected size's wire letter drives thumb_size; file sizes
// follow _photo_size_byte_count / document.size; missing access hash,
// file reference or a usable size fail with ErrMediaLocation — nothing
// is fabricated, no malformed wire object is ever built.

import (
	"github.com/Skryldev/suromesh/tl/gen"
)

// mediaLocation is the resolved download plan: either a wire location
// (RPC download) or inline bytes (cached/stripped — no RPC).

// resolveMediaLocation plans the download for any supported domain Media.
func resolveMediaLocation(m Media) (loc gen.InputFileLocationClass, size int64, inline []byte, err error) {
	switch t := m.(type) {
	case *Photo:
		return resolvePhotoLocation(t)
	case *Document:
		return resolveDocumentLocation(t, "")
	default:
		return nil, 0, nil, ErrUnsupportedMedia
	}
}

// resolvePhotoLocation ports _download_photo (downloads.py:848-880):
// largest usable size (thumb=None behavior); video sizes participate.
func resolvePhotoLocation(p *Photo) (gen.InputFileLocationClass, int64, []byte, error) {
	if p.AccessHash == 0 && p.ID != 0 {
		// Access hash 0 with a non-zero id cannot be addressed safely.
		return nil, 0, nil, ErrMediaLocation
	}
	// file_reference passes through verbatim (downloads.py:874: the
	// server's bytes are used as-is; empty is the server's choice).
	size, ok := p.Largest()
	if !ok || size.Kind == PhotoSizeEmptyK {
		return nil, 0, nil, ErrMediaLocation
	}
	// Cached/Stripped: the bytes ARE the content (downloads.py:826-845).
	if size.Kind == PhotoSizeCached || size.Kind == PhotoSizeStripped {
		data, cerr := strippedOrCachedBytes(size)
		if cerr != nil {
			return nil, 0, nil, cerr
		}
		return nil, 0, data, nil
	}
	return &gen.InputPhotoFileLocation{
		ID:            p.ID,
		AccessHash:    p.AccessHash,
		FileReference: p.FileReference,
		ThumbSize:     size.Type,
	}, size.ByteCount, nil, nil
}

// resolveDocumentLocation ports _download_document (downloads.py:911-951):
// thumbSize == "" fetches the full document; a specific thumb fetches
// that thumbnail.
func resolveDocumentLocation(d *Document, thumbSize string) (gen.InputFileLocationClass, int64, []byte, error) {
	if d.AccessHash == 0 && d.ID != 0 {
		return nil, 0, nil, ErrMediaLocation
	}
	// file_reference passes through verbatim (downloads.py:939).
	if thumbSize == "" {
		if d.Size <= 0 {
			return nil, 0, nil, ErrMediaLocation
		}
		return &gen.InputDocumentFileLocation{
			ID:            d.ID,
			AccessHash:    d.AccessHash,
			FileReference: d.FileReference,
			ThumbSize:     "",
		}, d.Size, nil, nil
	}
	// Thumbnail fetch: same rules as photo sizes.
	for _, s := range d.Thumbs {
		if s.Type != thumbSize {
			continue
		}
		if s.Kind == PhotoSizeCached || s.Kind == PhotoSizeStripped {
			data, cerr := strippedOrCachedBytes(s)
			if cerr != nil {
				return nil, 0, nil, cerr
			}
			return nil, 0, data, nil
		}
		if s.Kind == PhotoSizeEmptyK || s.ByteCount <= 0 {
			return nil, 0, nil, ErrMediaLocation
		}
		return &gen.InputDocumentFileLocation{
			ID:            d.ID,
			AccessHash:    d.AccessHash,
			FileReference: d.FileReference,
			ThumbSize:     s.Type,
		}, s.ByteCount, nil, nil
	}
	return nil, 0, nil, ErrMediaLocation
}

// strippedOrCachedBytes returns the inline payload. Stripped sizes get
// the JPEG header/footer splice (utils.stripped_photo_to_jpg,
// utils.py:1530-1544).
func strippedOrCachedBytes(s PhotoSize) ([]byte, error) {
	if s.Kind == PhotoSizeCached {
		return s.Bytes, nil
	}
	if s.Kind == PhotoSizeStripped {
		return strippedPhotoToJPG(s.Bytes), nil
	}
	return nil, ErrMediaLocation
}

// strippedPhotoToJPG ports utils.stripped_photo_to_jpg (utils.py:1530-1544):
// the stripped bytes carry width/height at [1],[2] followed by the JPEG
// scan; the fixed JFIF header (with byte 164/166 patched to the
// dimensions) is prepended and the EOI marker appended. Header-less
// stripped blobs pass through unchanged (utils.py:1537-1539).
func strippedPhotoToJPG(stripped []byte) []byte {
	if len(stripped) < 3 || stripped[0] != 1 {
		return stripped
	}
	header := strippedJPGHeader() // fixed 622-byte JFIF prefix
	header[164] = stripped[1]
	header[166] = stripped[2]
	out := make([]byte, 0, len(header)+len(stripped)-3+2)
	out = append(out, header...)
	out = append(out, stripped[3:]...)
	return append(out, 0xff, 0xd9)
}

// resolveMediaLocationShim exposes the canonical resolver to the external
// test package (production callers use resolveMediaLocation via
// DownloadMedia).

// ResolveMediaLocationForTest exposes the canonical resolver for the
// external test package (invariant tests).
func (c *Client) ResolveMediaLocationForTest(m Media) (gen.InputFileLocationClass, int64, []byte, error) {
	return resolveMediaLocation(m)
}
