package client

import (
	"context"
	"crypto/md5" // #nosec G401 — MD5 is the protocol-required part checksum for small files
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// randomInt64 generates a random int64 for file IDs (reference
// helpers.generate_random_long).
func randomInt64() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// UploadedFile is the domain result of a successful upload. It carries
// everything needed to reference the file in subsequent RPCs (SendMedia,
// SendFile). No access hash is fabricated — small files use MD5 for
// integrity; large files use the big-file input form which needs no hash.
type UploadedFile struct {
	ID     int64  // random file_id (protocol-level identifier)
	Parts  int32  // number of parts uploaded
	Size   int64  // total bytes
	Name   string // file name (may be auto-derived)
	IsBig  bool   // true when uploaded via saveBigFilePart (>10 MiB)
	MD5Sum string // hex MD5 for small files ("" for big files)
}

// AsInputFile converts the result to the wire input file for SendMedia.
func (u *UploadedFile) AsInputFile() gen.InputFileClass {
	if u.IsBig {
		return &gen.InputFileBig{ID: u.ID, Parts: u.Parts, Name: u.Name}
	}
	return &gen.InputFile{ID: u.ID, Parts: u.Parts, Name: u.Name, MD5Checksum: u.MD5Sum}
}

// UploadOptions configures one upload.
type UploadOptions struct {
	// FileName for the uploaded file (defaults to "file"). The reference
	// appends an extension based on MIME detection; here the caller
	// controls the name directly.
	FileName string
	// Size is the total byte count. Required for io.Reader that cannot
	// report it; when 0 and the reader is an io.Seeker, the size is
	// determined by seeking to the end.
	Size int64
	// PartSizeKB overrides the reference part-size policy (128/256/512 KB
	// by file size). Must be divisible by 1024 and ≤ 512.
	PartSizeKB int32
	// Progress receives periodic progress callbacks. Called from the
	// upload worker; must not block. Panics are recovered.
	Progress func(TransferProgress)
}

// Upload streams a file to Soroush Plus using the reference's part-upload
// protocol. The reader is consumed sequentially; memory usage is bounded
// by the part size (128–512 KiB), NOT by the file size.
//
// Protocol (reference uploads.py):
//   - file_id = random int64
//   - part_size = get_appropriated_part_size(file_size) — 128/256/512 KB
//   - small file (≤10 MiB): upload.saveFilePart per part, MD5 accumulated,
//     result = InputFile{id, parts, name, md5}
//   - large file (>10 MiB): upload.saveBigFilePart per part,
//     result = InputFileBig{id, parts, name}
//   - sequential (no parallel parts in the reference)
func (c *Client) Upload(ctx context.Context, r io.Reader, opts UploadOptions) (*UploadedFile, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}

	// Determine the file size: explicit option > io.Seeker > error for
	// non-seekable readers (the protocol needs a total part count).
	size := opts.Size
	if size == 0 {
		if s, ok := r.(interface{ Len() int }); ok {
			size = int64(s.Len())
		} else if sk, ok := r.(io.ReadSeeker); ok {
			cur, serr := sk.Seek(0, io.SeekCurrent)
			if serr != nil {
				return nil, fmt.Errorf("media: seek failed: %w", serr)
			}
			end, serr := sk.Seek(0, io.SeekEnd)
			if serr != nil {
				return nil, fmt.Errorf("media: seek failed: %w", serr)
			}
			size = end - cur
			if _, serr := sk.Seek(cur, io.SeekStart); serr != nil {
				return nil, fmt.Errorf("media: seek restore failed: %w", serr)
			}
			if size < 0 {
				return nil, ErrReaderShortRead
			}
		} else {
			return nil, ErrSizeUnknown
		}
	}
	if size < 0 {
		return nil, fmt.Errorf("%w: negative size %d", ErrInvalidConfig, size)
	}

	partSize, err := resolveUploadPartSize(uploadPartSize(size), opts.PartSizeKB)
	if err != nil {
		return nil, err
	}
	if partSize%1024 != 0 {
		return nil, fmt.Errorf("%w: %d not divisible by 1024", ErrInvalidPartSize, partSize)
	}

	isBig := size > bigFileThreshold
	partCount := int32((size + int64(partSize) - 1) / int64(partSize))
	if size == 0 {
		partCount = 0 // zero-length files upload zero parts (reference edge)
	}
	if isBig && partCount == 0 {
		partCount = 1 // big-file protocol requires at least one part
	}
	// The wire part count is int32; reject sizes whose part count would
	// wrap instead of letting arithmetic fabricate a smaller file.
	const maxUploadParts = 1 << 17 // 512 KB parts → 64 GiB ceiling
	if int64(partCount) > maxUploadParts {
		return nil, fmt.Errorf("%w: %d bytes → %d parts (max %d)", ErrFileTooLarge, size, partCount, maxUploadParts)
	}

	fileID := randomInt64()
	name := opts.FileName
	if name == "" {
		name = fmt.Sprintf("file_%d", fileID)
	}

	t, tCtx, release, err := c.transfers.begin(ctx, TransferUpload, size)
	if err != nil {
		return nil, err
	}
	t.state.Store(int32(TransferRunning))
	defer func() {
		// Terminal-CAS guard: a panic unwinding through Upload must still
		// settle the transfer exactly once (Phase 15 invariant: no slot
		// strand, no missing terminal state), then the panic propagates
		// to the caller (programmer error, never silently swallowed).
		if r := recover(); r != nil {
			c.transfers.finish(t, TransferFailed)
			release()
			panic(r)
		}
		release()
	}()

	result := &UploadedFile{
		ID:    fileID,
		Parts: partCount,
		Size:  size,
		Name:  name,
		IsBig: isBig,
	}

	// Sequential upload (reference-sequential); parts are idempotent so a
	// bounded retry with seek-back is safe for replayable readers.
	var md5Hash hashForMD5
	var uploaded int64

	buf := make([]byte, partSize)
	for partIdx := int32(0); partIdx < partCount; partIdx++ {
		if tCtx.Err() != nil {
			c.transfers.finish(t, TransferCancelled)
			return nil, fmt.Errorf("media: upload cancelled: %w", tCtx.Err())
		}

		// Read one part. Short reads are only valid on the last part.
		n, rerr := readFull(r, buf)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			c.transfers.finish(t, TransferFailed)
			return nil, fmt.Errorf("media: reader error at part %d/%d: %w", partIdx, partCount, rerr)
		}
		if n == 0 && partIdx < partCount-1 {
			c.transfers.finish(t, TransferFailed)
			return nil, fmt.Errorf("%w: got 0 bytes at part %d/%d (size %d)",
				ErrReaderShortRead, partIdx, partCount, size)
		}
		if n < int(partSize) && partIdx < partCount-1 {
			c.transfers.finish(t, TransferFailed)
			return nil, fmt.Errorf("%w: got %d bytes at part %d/%d (size %d)",
				ErrReaderShortRead, n, partIdx, partCount, size)
		}
		part := buf[:n]

		if !isBig {
			md5Hash.update(part)
		}

		// Upload the part with bounded retries for replayable readers.
		if err := c.uploadPart(tCtx, eng, fileID, partIdx, partCount, part, isBig, r, &uploaded, partSize); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				c.transfers.finish(t, TransferCancelled)
				return nil, fmt.Errorf("media: upload cancelled: %w", err)
			}
			c.transfers.finish(t, TransferFailed)
			return nil, &ErrPartUploadFailed{Part: partIdx, Total: partCount, Err: err}
		}

		uploaded += int64(n)
		atomic.StoreInt64(&t.written, uploaded)
		c.transfers.metrics.bytesUp.Add(uint64(n))

		// Progress callback (no manager lock held; panic isolated).
		c.transfers.reportProgress(t, opts.Progress, uploaded, size)
	}

	if !isBig {
		result.MD5Sum = md5Hash.hex()
	}

	// Integrity: uploaded bytes must match the declared size.
	if uploaded != size {
		c.transfers.finish(t, TransferFailed)
		return nil, fmt.Errorf("%w: uploaded %d bytes, expected %d", ErrTransferIntegrity, uploaded, size)
	}

	c.transfers.finish(t, TransferCompleted)
	return result, nil
}

// uploadPart uploads one part with bounded retry (reference: no retry —
// part failure is fatal; we allow one retry for transient network errors
// when the reader can replay via Seek).
func (c *Client) uploadPart(ctx context.Context, eng *rpc.Engine,
	fileID int64, partIdx, partCount int32, part []byte, isBig bool,
	r io.Reader, uploaded *int64, partSize int32,
) error {
	const maxAttempts = 2
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Replay: seek back to the part start. Only safe for
			// io.ReadSeeker; non-replayable readers fail on the first
			// attempt (deterministic — no fabricated data).
			sk, ok := r.(io.ReadSeeker)
			if !ok {
				return fmt.Errorf("%w: cannot retry part %d", ErrNonReplayableReader, partIdx)
			}
			offset := int64(partIdx) * int64(partSize)
			if _, serr := sk.Seek(offset, io.SeekStart); serr != nil {
				return fmt.Errorf("media: seek to %d failed: %w", offset, serr)
			}
			// Re-read the part.
			buf := make([]byte, len(part))
			n, rerr := readFull(sk, buf)
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return fmt.Errorf("media: reader replay error: %w", rerr)
			}
			part = buf[:n]
			c.transfers.metrics.retriesUp.Add(1)
		}

		var req tl.Request
		if isBig {
			req = &gen.UploadSaveBigFilePartRequest{
				FileID:         fileID,
				FilePart:       partIdx,
				FileTotalParts: partCount,
				Bytes:          part,
			}
		} else {
			req = &gen.UploadSaveFilePartRequest{
				FileID:   fileID,
				FilePart: partIdx,
				Bytes:    part,
			}
		}

		res, err := eng.Invoke(ctx, req)
		if err == nil {
			if b, ok := res.(*gen.BoolTrue); ok && b != nil {
				return nil
			}
			// saveFilePart returns Bool; False means the server rejected
			// the part (protocol-level failure).
			return errors.New("media: server returned False for part upload")
		}
		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		var rpcErr *rpc.RPCError
		if errors.As(err, &rpcErr) {
			return err // RPC errors are not retried at the part level
		}
		// Network errors: retry once.
	}
	return lastErr
}

// readFull reads until buf is full or EOF (io.ReadFull without the
// ErrUnexpectedEOF distinction — we handle short reads explicitly).
func readFull(r io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, io.EOF
			}
			return total, err
		}
		if n == 0 {
			return total, io.EOF
		}
	}
	return total, nil
}

// hashForMD5 abstracts the MD5 accumulator (protocol-required for small
// file uploads; NOT a general security choice).
type hashForMD5 struct {
	mh interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
}

func (h *hashForMD5) update(b []byte) {
	if h.mh == nil {
		h.mh = md5.New() // #nosec G401 — protocol-required
	}
	_, _ = h.mh.Write(b)
}

func (h *hashForMD5) hex() string {
	if h.mh == nil {
		return hex.EncodeToString(md5.New().Sum(nil)) // #nosec G401
	}
	return hex.EncodeToString(h.mh.Sum(nil))
}
