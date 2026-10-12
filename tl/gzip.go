package tl

import (
	"bytes"
	"compress/gzip"
	"io"
)

// Gzip compresses data at the default compression level.
//
// Compressed bytes are not stable across implementations (zlib vs isal vs
// Go's compress/gzip); only decompression is wire-guaranteed. The sender
// applies gzip only when it makes the payload strictly smaller, and the
// server decompresses before decoding, so differing compressed bytes are
// protocol-legal.
func Gzip(data []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	// gzip.NewWriter cannot fail on a bytes.Buffer; Write errors are
	// impossible here too, but be explicit for vet.
	if _, err := zw.Write(data); err != nil {
		return nil
	}
	if err := zw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// Gunzip decompresses a gzip payload with a bounded output size to resist
// decompression bombs: limit bounds the decompressed output.
func GunzipLimit(packed []byte, limit int) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out := make([]byte, 0, min(len(packed)*4, limit))
	lr := io.LimitReader(zr, int64(limit)+1)
	out, err = io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if len(out) > limit {
		return nil, ErrPayloadTooLarge
	}
	return out, nil
}

// Gunzip decompresses with the default transport bound (16 MiB, matching the
// reference implementation's WebSocket max_msg_size).
func Gunzip(packed []byte) ([]byte, error) {
	return GunzipLimit(packed, 16<<20)
}
