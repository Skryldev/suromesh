package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl/gen"
)

// DownloadOptions configures one download.
type DownloadOptions struct {
	// Offset is the byte offset to start downloading from (default 0).
	Offset int64
	// Size is the total byte count to download (0 = until EOF, detected
	// by a short chunk from the server).
	Size int64
	// PartSizeKB overrides the download limit per chunk (must be divisible
	// by 4096, ≤ 512 KB; defaults per the reference's part-size policy).
	PartSizeKB int32
	// Progress receives periodic progress callbacks. Must not block.
	Progress func(TransferProgress)
}

// Download streams a file from Soroush Plus into w. Memory usage is
// bounded by the chunk size (64–512 KiB), NOT by the file size.
//
// Protocol (reference downloads.py):
//   - limit = part_size_kb * 1024, must be %4096 == 0, ≤ 512 KB
//   - offset must be aligned to limit (the reference subtracts
//     offset % limit before fetching and skips the prefix)
//   - download terminates when a chunk is shorter than the limit (EOF)
//   - upload.getFile(location, offset, limit) is the only RPC
func (c *Client) Download(ctx context.Context, location gen.InputFileLocationClass, w io.Writer, opts DownloadOptions) error {
	eng, err := c.engine()
	if err != nil {
		return err
	}
	if location == nil {
		return errors.New("media: nil file location")
	}
	if w == nil {
		return errors.New("media: nil writer")
	}
	if opts.Offset < 0 {
		return fmt.Errorf("media: negative offset %d: %w", opts.Offset, ErrInvalidConfig)
	}

	chunkSize, err := resolveDownloadChunkSize(downloadChunkSize(opts.Size), opts.PartSizeKB)
	if err != nil {
		return err
	}

	// Per-transfer context: bounded by the caller's ctx + lifecycle.
	t, tCtx, release, err := c.transfers.begin(ctx, TransferDownload, opts.Size)
	if err != nil {
		return err
	}
	t.state.Store(int32(TransferRunning))
	defer func() {
		// Terminal-CAS guard (Phase 15): mirror of the Upload guard.
		if r := recover(); r != nil {
			c.transfers.finish(t, TransferFailed)
			release()
			panic(r)
		}
		release()
	}()

	// Reference alignment: subtract offset % limit before fetching, skip
	// the prefix bytes in the first chunk.
	offset := opts.Offset
	bad := offset % int64(chunkSize)
	alignedOffset := offset - bad
	if alignedOffset < 0 {
		alignedOffset = 0
		bad = offset
	}

	var written int64
	if bad > 0 {
		// The first chunk starts at alignedOffset and includes `bad` bytes
		// of unwanted prefix — read them and discard. A short or empty
		// chunk here is EOF: the file ends inside the skipped prefix.
		// Sized downloads write at most the remaining window.
		remaining := opts.Size
		prefixErr := c.downloadRange(tCtx, eng, t, location, alignedOffset, chunkSize, func(chunk []byte) error {
			rest := chunk
			if int64(len(rest)) > bad {
				rest = rest[bad:]
			} else {
				rest = nil
			}
			if remaining > 0 && int64(len(rest)) > remaining {
				rest = rest[:remaining]
			}
			if len(rest) == 0 {
				return nil
			}
			n, werr := w.Write(rest)
			written += int64(n)
			atomic.AddInt64(&t.written, int64(n))
			return werr
		}, true)
		if prefixErr != nil && !errors.Is(prefixErr, io.EOF) {
			c.transfers.finish(t, classifyTransferEnd(tCtx, prefixErr))
			return prefixErr
		}
		if errors.Is(prefixErr, io.EOF) {
			// Nothing beyond the prefix: done.
			if opts.Size > 0 && written != opts.Size {
				c.transfers.finish(t, TransferFailed)
				return fmt.Errorf("%w: downloaded %d bytes, expected %d", ErrTransferIntegrity, written, opts.Size)
			}
			c.transfers.finish(t, TransferCompleted)
			return nil
		}
		if opts.Size > 0 && written >= opts.Size {
			// The window was satisfied inside the aligned prefix chunk.
			c.transfers.finish(t, TransferCompleted)
			return nil
		}
		// Continue from the first clean offset after the skipped prefix.
		// `written` already carries the useful bytes from the prefix chunk.
		offset = alignedOffset + int64(chunkSize)
	}

	// Sequential chunks from the aligned/clean offset.
	current := offset
	for {
		if tCtx.Err() != nil {
			c.transfers.finish(t, TransferCancelled)
			return fmt.Errorf("media: download cancelled: %w", tCtx.Err())
		}

		var remaining int64 = -1 // bytes still expected (sized downloads only)
		limit := chunkSize
		if opts.Size > 0 {
			remaining = opts.Size - written
			if remaining <= 0 {
				break
			}
			if int64(limit) > remaining {
				limit = int32(remaining)
				// Round up to the 4 KiB granularity the protocol requires.
				if limit%minDownloadChunk != 0 {
					limit = ((limit / minDownloadChunk) + 1) * minDownloadChunk
				}
			}
		}

		var n int64
		err := c.downloadRange(tCtx, eng, t, location, current, limit, func(chunk []byte) error {
			// The final limit is rounded up to the protocol's 4 KiB
			// granularity; a server holding more data past the window may
			// answer with the full rounded limit — never write past Size.
			if remaining >= 0 && int64(len(chunk)) > remaining {
				chunk = chunk[:remaining]
			}
			nw, werr := w.Write(chunk)
			n = int64(nw)
			written += int64(nw)
			atomic.AddInt64(&t.written, int64(nw))
			return werr
		}, false)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Short chunk = EOF; the download is complete.
				break
			}
			c.transfers.finish(t, classifyTransferEnd(tCtx, err))
			return err
		}
		if n == 0 {
			break // EOF
		}

		c.transfers.metrics.bytesDown.Add(uint64(n))
		c.transfers.reportProgress(t, opts.Progress, written, opts.Size)
		current += int64(limit)

		if opts.Size > 0 && written >= opts.Size {
			break
		}
		if n < int64(limit) {
			break // short chunk = EOF
		}
	}

	// Integrity: verify the transferred byte count when the size is known.
	if opts.Size > 0 && written != opts.Size {
		c.transfers.finish(t, TransferFailed)
		return fmt.Errorf("%w: downloaded %d bytes, expected %d", ErrTransferIntegrity, written, opts.Size)
	}

	c.transfers.finish(t, TransferCompleted)
	return nil
}

// downloadRange fetches one chunk at the given offset and delivers the
// useful bytes to writeFn. Returns io.EOF when the chunk is shorter than
// the limit (end of file).
func (c *Client) downloadRange(ctx context.Context, eng *rpc.Engine,
	t *transfer, location gen.InputFileLocationClass, offset int64, limit int32,
	writeFn func([]byte) error, skipPrefix bool,
) error {
	req := &gen.UploadGetFileRequest{
		Location: location,
		Offset:   offset,
		Limit:    limit,
	}
	res, err := eng.Invoke(ctx, req)
	if err != nil {
		return err
	}
	file, ok := res.(*gen.UploadFile)
	if !ok {
		return fmt.Errorf("media: getFile answered %T", res)
	}
	if len(file.Bytes) == 0 {
		return io.EOF
	}
	if int32(len(file.Bytes)) < limit {
		// Short chunk = final chunk; the caller detects EOF from the
		// returned byte count.
		if writeFn != nil {
			if werr := writeFn(file.Bytes); werr != nil {
				return werr
			}
		}
		return io.EOF
	}
	if writeFn != nil {
		return writeFn(file.Bytes)
	}
	return nil
}

// classifyTransferEnd maps an error to the terminal transfer state.
func classifyTransferEnd(ctx context.Context, err error) TransferState {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return TransferCancelled
	}
	return TransferFailed
}
