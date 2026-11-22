package handlers

// MediaService handlers: client-streaming upload (first frame metadata,
// ordered chunks), server-streaming download. Bounded memory: chunks flow
// through an io.Pipe into the SDK's O(part) upload path — nothing buffers
// the whole file.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

const (
	maxUploadBytes  = int64(2) << 30 // 2 GiB (big-file upstream path)
	maxChunkBytes   = 256 << 10      // 256 KiB server-side chunk clamp
	minChunkBytes   = 64 << 10       // download chunk floor
	maxChunkBytesDl = 512 << 10      // download chunk ceiling
)

func (h *RuntimeHandlers) GetMediaMetadata(ctx context.Context, req *v1.GetMediaMetadataRequest) (*v1.GetMediaMetadataResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	id, err := parseMessageID(req.GetMessageId())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var summary domain.MediaSummary
	err = r.Read(ctx, func(s runtimeSession) error {
		summary, err = s.GetMediaMetadata(ctx, ref, id)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.GetMediaMetadataResponse{
		Media:              convertMediaSummary(&summary),
		FileReferenceState: "unknown",
	}, nil
}

// UploadMedia implements the client-streaming contract. First frame =
// metadata (size required by the upstream); later frames = ordered
// chunks. Partial uploads are discarded on error/cancellation.
func (h *RuntimeHandlers) UploadMedia(stream v1.MediaService_UploadMediaServer) error {
	p, err := principal(stream.Context())
	if err != nil {
		return err
	}

	var start *v1.UploadMediaStart
	pr, pw := io.Pipe()
	done := make(chan struct{})
	var uploadErr error

	go func() {
		defer close(done)
	}()

	// Read the first frame (metadata).
	frame, ferr := stream.Recv()
	if ferr != nil {
		return mapError(ferr)
	}
	start = frame.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "first frame must be the start frame")
	}
	if err := validateUploadStart(start); err != nil {
		return mapError(err)
	}

	r, err := h.runner(stream.Context(), p.TenantID, start.GetAccountId())
	if err != nil {
		return mapError(err)
	}

	// Feed chunks through a pipe into the SDK upload (running in the
	// mutation lane: media transfer is an upstream mutation).
	spec := portsUploadSpec(start)
	resultC := make(chan uploadResult, 1)
	go func() {
		defer close(done)
		var msg domain.Message
		err := r.Mutate(stream.Context(), func(s runtimeSession) error {
			msg, err = s.UploadMedia(stream.Context(), spec, pr)
			return err
		})
		if err != nil {
			uploadErr = err
			_ = pw.CloseWithError(err)
			resultC <- uploadResult{err: err}
			return
		}
		_ = pw.Close()
		resultC <- uploadResult{msg: msg}
	}()

	var received int64
	for {
		frame, ferr := stream.Recv()
		if ferr != nil {
			// Client cancelled/ended early: discard the partial upload.
			_ = pw.CloseWithError(fmt.Errorf("%w: client ended stream early",
				domain.ErrInvalidInput))
			<-done
			return status.Error(codes.InvalidArgument, "upload ended before the declared size was reached")
		}
		chunk := frame.GetChunk()
		if chunk == nil {
			_ = pw.CloseWithError(fmt.Errorf("%w: chunk frame expected", domain.ErrInvalidInput))
			<-done
			return status.Error(codes.InvalidArgument, "chunk frame expected")
		}
		if len(chunk) > maxChunkBytes {
			_ = pw.CloseWithError(fmt.Errorf("%w: chunk %d exceeds %d bytes",
				domain.ErrInvalidInput, len(chunk), maxChunkBytes))
			<-done
			return status.Error(codes.InvalidArgument, "chunk too large")
		}
		received += int64(len(chunk))
		if received > start.GetSizeBytes() {
			_ = pw.CloseWithError(fmt.Errorf("%w: received %d bytes exceeds declared %d",
				domain.ErrInvalidInput, received, start.GetSizeBytes()))
			<-done
			return status.Error(codes.InvalidArgument, "received bytes exceed declared size")
		}
		if _, werr := pw.Write(chunk); werr != nil {
			<-done
			return mapError(uploadErr)
		}
		if received == start.GetSizeBytes() {
			break // complete: close pipe (clean EOF for the uploader)
		}
	}
	_ = pw.Close()
	<-done

	res := <-resultC
	if res.err != nil {
		return mapError(res.err)
	}
	return stream.SendAndClose(&v1.UploadMediaResponse{Message: convertDomainMessage(res.msg)})
}

type portsUploadSpecT = ports.UploadSpec

type uploadResult struct {
	msg domain.Message
	err error
}

func validateUploadStart(start *v1.UploadMediaStart) error {
	if start.GetSizeBytes() <= 0 || start.GetSizeBytes() > maxUploadBytes {
		return fmt.Errorf("%w: size_bytes out of range (1..%d)", domain.ErrInvalidInput, maxUploadBytes)
	}
	if start.GetFileName() == "" || len(start.GetFileName()) > 255 {
		return fmt.Errorf("%w: file_name required (1..255 chars)", domain.ErrInvalidInput)
	}
	if start.GetMimeType() == "" || len(start.GetMimeType()) > 127 {
		return fmt.Errorf("%w: mime_type required (1..127 chars)", domain.ErrInvalidInput)
	}
	if err := requireAccountID(start.GetAccountId()); err != nil {
		return err
	}
	if _, err := parsePeerRef(start.GetPeer()); err != nil {
		return err
	}
	return nil
}

func portsUploadSpec(start *v1.UploadMediaStart) ports.UploadSpec {
	ref, _ := parsePeerRef(start.GetPeer())
	return ports.UploadSpec{
		Peer: ref, SizeBytes: start.GetSizeBytes(), FileName: start.GetFileName(),
		MimeType: start.GetMimeType(), AsDocument: start.GetAsDocument(),
	}
}

func (h *RuntimeHandlers) DownloadMedia(req *v1.DownloadMediaRequest, stream v1.MediaService_DownloadMediaServer) error {
	p, err := principal(stream.Context())
	if err != nil {
		return err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return mapError(err)
	}
	id, err := parseMessageID(req.GetMessageId())
	if err != nil {
		return mapError(err)
	}
	chunk := int(req.GetChunkSizeHint())
	if chunk < minChunkBytes {
		chunk = minChunkBytes
	}
	if chunk > maxChunkBytesDl {
		chunk = maxChunkBytesDl
	}
	r, err := h.runner(stream.Context(), p.TenantID, req.GetAccountId())
	if err != nil {
		return mapError(err)
	}

	pr, pw := io.Pipe()
	errC := make(chan error, 1)
	go func() {
		errC <- r.Read(stream.Context(), func(s runtimeSession) error {
			return s.DownloadMedia(stream.Context(), ref, id, pw)
		})
		_ = pw.Close()
	}()

	buf := make([]byte, chunk)
	var offset int64
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			if serr := stream.Send(&v1.DownloadChunk{Data: buf[:n], Offset: offset}); serr != nil {
				return nil // transport closed
			}
			offset += int64(n)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				if gerr := <-errC; gerr != nil {
					return mapError(gerr)
				}
				return nil
			}
			<-errC
			return mapError(rerr)
		}
	}
}
