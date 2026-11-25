package server_test

// Streaming + media contract tests (Phase 26 Layer D §4.4/§4.8/§4.9):
// in-process gRPC server over the fake upstream. Delivery semantics,
// bounded buffers, deadline behavior, media validation.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// streamingFixture: a fixture whose account has a STARTED runner with a
// fake client, so streaming/media RPCs reach the runtime.
func streamingFixture(t *testing.T) *fixture {
	fx := newFixture(t)
	ctx := authed(context.Background())
	bind, err := fx.accountClient().BindAccount(ctx, &v1.BindAccountRequest{
		Phone: "+15550001111", DisplayAlias: "stream-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.manager.StartRunner(ctx, application.RunnerConfig{
		AccountID:    domain.AccountID(bind.Account.AccountId),
		TenantID:     "tenant-1",
		StreamBuffer: 4,
	}); err != nil {
		t.Fatal(err)
	}
	fx.boundID = bind.Account.AccountId
	return fx
}

func TestStreamUpdates_DeliversAndSequences(t *testing.T) {
	fx := streamingFixture(t)
	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()

	stream, err := fx.updateClient().StreamUpdates(ctx, &v1.StreamUpdatesRequest{
		AccountId: fx.boundID,
	})
	if err != nil {
		t.Fatal(err)
	}

	client := fx.factory.ClientFor(domain.AccountID(fx.boundID))
	if client == nil {
		t.Fatal("fake client missing")
	}
	// Allow the server handler to register its hub subscriber before
	// publishing (RPC-setup race; publish-then-subscribe loses events by
	// the documented at-most-once semantics).
	time.Sleep(150 * time.Millisecond)
	for i := 0; i < 3; i++ {
		client.Publish(domain.UpdateEvent{Kind: domain.UpdateNewMessage,
			NewMessage: &domain.Message{ID: int64(i + 1), Text: "e"}})
	}

	got := 0
	var lastSeq int64
	for got < 3 {
		ev, rerr := stream.Recv()
		if rerr != nil {
			t.Fatalf("recv after %d events: %v", got, rerr)
		}
		got++
		if ev.StreamSequence <= lastSeq {
			t.Fatalf("sequence not monotonic: %d after %d", ev.StreamSequence, lastSeq)
		}
		lastSeq = ev.StreamSequence
		if ev.GetNewMessage() == nil {
			t.Fatalf("event %d: missing message", got)
		}
	}
}

func TestStreamUpdates_ClientCancelEndsCleanly(t *testing.T) {
	fx := streamingFixture(t)
	ctx, cancel := context.WithTimeout(authed(context.Background()), 10*time.Second)
	stream, err := fx.updateClient().StreamUpdates(ctx, &v1.StreamUpdatesRequest{
		AccountId: fx.boundID,
	})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not end after client cancellation")
	}
}

func TestStreamHistory_UnscriptedFailsHonest(t *testing.T) {
	fx := streamingFixture(t)
	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()
	stream, err := fx.messagingClient().StreamHistory(ctx, &v1.StreamHistoryRequest{
		AccountId: fx.boundID,
		Peer:      &v1.PeerRef{Ref: &v1.PeerRef_Self{Self: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("unscripted history must not fabricate messages")
	}
}

func TestMediaUpload_FirstFrameMustBeMetadata(t *testing.T) {
	fx := streamingFixture(t)
	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()
	stream, err := fx.mediaClient().UploadMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Chunk{Chunk: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestMediaUpload_MetadataValidation(t *testing.T) {
	fx := streamingFixture(t)
	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()
	cases := []struct {
		name string
		req  *v1.UploadMediaStart
		want codes.Code
	}{
		{"zero size", &v1.UploadMediaStart{AccountId: fx.boundID, SizeBytes: 0, FileName: "f", MimeType: "a/b"}, codes.InvalidArgument},
		{"oversize", &v1.UploadMediaStart{AccountId: fx.boundID, SizeBytes: (2 << 30) + 1, FileName: "f", MimeType: "a/b"}, codes.InvalidArgument},
		{"no filename", &v1.UploadMediaStart{AccountId: fx.boundID, SizeBytes: 10, MimeType: "a/b"}, codes.InvalidArgument},
		{"no mime", &v1.UploadMediaStart{AccountId: fx.boundID, SizeBytes: 10, FileName: "f"}, codes.InvalidArgument},
		{"bad account", &v1.UploadMediaStart{AccountId: "nope", SizeBytes: 10, FileName: "f", MimeType: "a/b"}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := fx.mediaClient().UploadMedia(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Start{Start: tc.req}}); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.CloseAndRecv(); status.Code(err) != tc.want {
				t.Fatalf("code = %s, want %s", status.Code(err), tc.want)
			}
		})
	}
}

func TestMediaUpload_SizeEnforcement(t *testing.T) {
	fx := streamingFixture(t)
	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()
	stream, err := fx.mediaClient().UploadMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Start{Start: &v1.UploadMediaStart{
		AccountId: fx.boundID, SizeBytes: 4, FileName: "f", MimeType: "a/b",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Chunk{Chunk: []byte("12345678")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestMediaUpload_UnscriptedUpstreamFailsHonest(t *testing.T) {
	fx := streamingFixture(t)
	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()
	stream, err := fx.mediaClient().UploadMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Start{Start: &v1.UploadMediaStart{
		AccountId: fx.boundID, SizeBytes: 2, FileName: "f", MimeType: "a/b",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Chunk{Chunk: []byte("ok")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.CloseAndRecv(); err == nil {
		t.Fatal("unscripted upload must fail, not fabricate success")
	}
}

func TestDeadline_CallerDeadlineWins(t *testing.T) {
	fx := newFixture(t)
	client := fx.accountClient()
	ctx, cancel := context.WithTimeout(authed(context.Background()), time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)
	_, err := client.ListAccounts(ctx, &v1.ListAccountsRequest{})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("code = %s, want DeadlineExceeded", status.Code(err))
	}
}

var (
	_ = io.EOF
	_ = errors.New
)

// ---- Phase 27 media-path regressions ---------------------------------------
//
// These tests force the UPLOAD WORKER GOROUTINE to run to completion and
// the DOWNLOAD worker to be released on client cancel. They pin two Phase
// 27 fixes in handlers/media.go:
//   1. `done` is closed exactly once by the upload worker (the earlier
//      stray goroutine made any completed upload a nondeterministic
//      close-of-closed-channel process crash and left `<-done` decoupled
//      from worker completion);
//   2. a cancelled download closes the pipe so the SDK-side writer
//      unblocks (no leaked worker goroutine / read slot).

func TestMediaUpload_ScriptedSuccessCompletesWorker(t *testing.T) {
	fx := streamingFixture(t)
	client := fx.factory.ClientFor(domain.AccountID(fx.boundID))
	if client == nil {
		t.Fatal("fake client missing")
	}
	const payload = "hello-media"
	var uploaded []byte
	client.OnUploadMedia = func(spec ports.UploadSpec, r io.Reader) (domain.Message, error) {
		b, err := io.ReadAll(r)
		if err != nil {
			return domain.Message{}, err
		}
		uploaded = b
		peer, _ := domain.ParsePeerID("user:42")
		return domain.Message{ID: 7, PeerID: peer, Text: spec.FileName}, nil
	}

	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()
	stream, err := fx.mediaClient().UploadMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Start{Start: &v1.UploadMediaStart{
		AccountId: fx.boundID, Peer: &v1.PeerRef{Ref: &v1.PeerRef_Self{Self: true}},
		SizeBytes: int64(len(payload)), FileName: "f.bin", MimeType: "application/octet-stream",
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Chunk{Chunk: []byte(payload)}}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Fatalf("scripted upload must succeed end-to-end: %v", err)
	}
	if resp.Message == nil || resp.Message.MessageId != "7" {
		t.Fatalf("response message mismatch: %+v", resp.Message)
	}
	if string(uploaded) != payload {
		t.Fatalf("worker received %q, want %q (full streaming path)", uploaded, payload)
	}
}

func TestMediaUpload_ScriptedWorkerFailureMapped(t *testing.T) {
	fx := streamingFixture(t)
	client := fx.factory.ClientFor(domain.AccountID(fx.boundID))
	client.OnUploadMedia = func(spec ports.UploadSpec, r io.Reader) (domain.Message, error) {
		// Drain the pipe so the handler's writes never block, then fail
		// like an upstream rejection would.
		_, _ = io.Copy(io.Discard, r)
		return domain.Message{}, fmt.Errorf("%w: scripted upstream rejection", domain.ErrUpstreamUnavailable)
	}

	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	defer cancel()
	stream, err := fx.mediaClient().UploadMedia(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Start{Start: &v1.UploadMediaStart{
		AccountId: fx.boundID, Peer: &v1.PeerRef{Ref: &v1.PeerRef_Self{Self: true}},
		SizeBytes: 4, FileName: "f.bin", MimeType: "application/octet-stream",
	}}})
	_ = stream.Send(&v1.UploadMediaFrame{Frame: &v1.UploadMediaFrame_Chunk{Chunk: []byte("data")}})
	if _, err := stream.CloseAndRecv(); err == nil {
		t.Fatal("scripted upstream failure must surface honestly")
	}
	// The worker completed (error propagated); the process did not crash
	// on a double close — that is the assertion (a panic here fails the
	// whole test binary).
}

func TestMediaDownload_ClientCancelReleasesWorker(t *testing.T) {
	fx := streamingFixture(t)
	client := fx.factory.ClientFor(domain.AccountID(fx.boundID))
	workerDone := make(chan struct{})
	var wrote int
	client.OnDownloadMedia = func(ref domain.PeerRef, id int64, w io.Writer) error {
		defer close(workerDone)
		// Write until the consumer side goes away (pipe closed): exactly
		// the shape of a long real download interrupted by the client.
		payload := bytes.Repeat([]byte{0xAB}, 1024)
		for {
			if _, err := w.Write(payload); err != nil {
				return err // pipe closed by cancel path → unblocks
			}
			wrote++
		}
	}

	ctx, cancel := context.WithTimeout(authed(context.Background()), 5*time.Second)
	stream, err := fx.mediaClient().DownloadMedia(ctx, &v1.DownloadMediaRequest{
		AccountId: fx.boundID, Peer: &v1.PeerRef{Ref: &v1.PeerRef_Self{Self: true}}, MessageId: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Read a couple of chunks, then cancel mid-stream.
	for i := 0; i < 2; i++ {
		if _, err := stream.Recv(); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
	cancel()
	// The worker must be released (pipe closed) — bounded wait.
	select {
	case <-workerDone:
		if wrote < 2 {
			t.Fatalf("worker wrote only %d blocks", wrote)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("download worker leaked: pipe was not closed on client cancel")
	}
}
