package client

import (
	"errors"
	"fmt"
)

// Media/transfer error sentinels. All support errors.Is/As.
var (
	// ErrTransferClosed is returned when the transfer manager is shut down.
	ErrTransferClosed = errors.New("media: transfer manager closed")
	// ErrUnsupportedLocation is returned for file locations the library
	// does not support (the schema defines them but the reference
	// behavior does not exercise them in this phase).
	ErrUnsupportedLocation = errors.New("media: unsupported file location")
	// ErrNonReplayableReader is returned when a retry requires seeking
	// back but the reader only implements io.Reader (no replay).
	ErrNonReplayableReader = errors.New("media: reader is not replayable (io.ReadSeeker required for retry)")
	// ErrReaderShortRead is returned when the reader returns fewer bytes
	// than expected before the last part.
	ErrReaderShortRead = errors.New("media: reader returned fewer bytes than expected")
	// ErrReaderTooManyBytes is returned when the declared file size is
	// smaller than the actual reader output.
	ErrReaderTooManyBytes = errors.New("media: reader produced more bytes than declared file size")
	// ErrTransferIntegrity is returned when the transferred byte count
	// does not match the expected size.
	ErrTransferIntegrity = errors.New("media: transfer integrity check failed")
	// ErrInvalidPartSize is returned for part sizes that violate the
	// protocol constraints.
	ErrInvalidPartSize = errors.New("media: invalid part size")
	// ErrFileTooLarge is returned when the file exceeds the protocol
	// maximum.
	ErrFileTooLarge = errors.New("media: file too large")
	// ErrZeroSizeFileWithUnknownLength is returned when neither the caller
	// nor the reader can provide the file size for a batch part transfer.
	ErrSizeUnknown = errors.New("media: file size unknown (provide UploadOptions.Size or use io.ReadSeeker)")
)

// ErrPartUploadFailed reports a failed part upload with its index.
type ErrPartUploadFailed struct {
	Part  int32
	Total int32
	Err   error
}

func (e *ErrPartUploadFailed) Error() string {
	return fmt.Sprintf("media: failed to upload part %d/%d: %v", e.Part, e.Total, e.Err)
}

func (e *ErrPartUploadFailed) Unwrap() error { return e.Err }

// ErrDownloadShortChunk reports a download chunk shorter than the
// requested limit that is not the final chunk.
type ErrDownloadShortChunk struct {
	Offset   int64
	Expected int32
	Got      int32
}

func (e *ErrDownloadShortChunk) Error() string {
	return fmt.Sprintf("media: short chunk at offset %d: got %d bytes, want %d", e.Offset, e.Got, e.Expected)
}

// partSizeForSize implements the reference part-size policy
// (utils.get_appropriated_part_size): returns KB.
func partSizeKBForSize(fileSize int64) int32 {
	switch {
	case fileSize <= 104857600: // 100 MB
		return 128
	case fileSize <= 786432000: // 750 MB
		return 256
	default:
		return 512
	}
}

// maxUploadPartSize is the protocol's part size cap (512 KB).
const maxUploadPartSize = 512 * 1024

// bigFileThreshold matches the reference's 10 MiB small/large split.
const bigFileThreshold = 10 * 1024 * 1024

// minDownloadChunk is the download offset granularity (4 KiB).
const minDownloadChunk = 4096

// maxDownloadChunk is the download limit cap (512 KiB).
const maxDownloadChunk = 512 * 1024

// uploadPartSize computes the part size for an upload per the reference:
//
//	part_size_kb = get_appropriated_part_size(file_size)  // 128/256/512
//	part_size    = part_size_kb * 1024                    // %1024 == 0
//	cap          = 512 KB
func uploadPartSize(fileSize int64) int32 {
	kb := partSizeKBForSize(fileSize)
	ps := kb * 1024
	if ps > maxUploadPartSize {
		ps = maxUploadPartSize
	}
	return ps
}

// downloadChunkSize computes the download limit per the reference
// (downloads.py: part_size_kb defaults to 64 when no file size, else
// get_appropriated_part_size; must be %4096 == 0; cap 512 KB).
func downloadChunkSize(fileSize int64) int32 {
	if fileSize <= 0 {
		return 64 * 1024 // reference: "Reasonable default"
	}
	kb := partSizeKBForSize(fileSize)
	ps := kb * 1024
	if ps > maxDownloadChunk {
		ps = maxDownloadChunk
	}
	if ps%minDownloadChunk != 0 {
		ps = (ps / minDownloadChunk) * minDownloadChunk
	}
	return ps
}

// resolveUploadPartSize applies UploadOptions.PartSizeKB on top of the
// size-derived default. The arithmetic is int64 on purpose: kb*1024 wraps
// int32 for absurd inputs (4194304*1024 → 0), which would otherwise slip
// past the divisibility check and blow up downstream. Fuzz seam.
func resolveUploadPartSize(defaultSize int32, kb int32) (int32, error) {
	switch {
	case kb > 512:
		return 0, fmt.Errorf("%w: %d KB (max 512)", ErrInvalidPartSize, kb)
	case kb > 0:
		ps := int64(kb) * 1024
		if ps > maxUploadPartSize {
			return 0, fmt.Errorf("%w: %d bytes (max %d)", ErrInvalidPartSize, ps, maxUploadPartSize)
		}
		return int32(ps), nil
	case kb < 0:
		return 0, fmt.Errorf("%w: negative PartSizeKB %d", ErrInvalidPartSize, kb)
	}
	return defaultSize, nil
}

// resolveDownloadChunkSize is resolveUploadPartSize's download twin: the
// wire requires limits divisible by 4096 and ≤ 512 KB. Fuzz seam.
func resolveDownloadChunkSize(defaultSize int32, kb int32) (int32, error) {
	switch {
	case kb > 0:
		ps := int64(kb) * 1024
		if ps%minDownloadChunk != 0 {
			return 0, fmt.Errorf("%w: %d not divisible by %d", ErrInvalidPartSize, ps, minDownloadChunk)
		}
		if ps > maxDownloadChunk {
			return 0, fmt.Errorf("%w: %d (max %d)", ErrInvalidPartSize, ps, maxDownloadChunk)
		}
		return int32(ps), nil
	case kb < 0:
		return 0, fmt.Errorf("%w: negative PartSizeKB %d", ErrInvalidPartSize, kb)
	}
	return defaultSize, nil
}
