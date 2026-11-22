package server_test

// Streaming + media contract tests (Phase 26 Layer D §4.4/§4.8/§4.9):
// in-process gRPC server over the fake upstream. Delivery semantics,
// bounded buffers, deadline behavior, media validation.

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/domain"
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
