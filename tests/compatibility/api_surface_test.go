// Public API surface contract (Phase 17).
//
// This file is a compile-time pin of the consumer-facing API shapes plus a
// behavioral pin of the documented INTERNAL test seams. If any signature
// below stops compiling, the public API changed: that must be a deliberate,
// documented decision (docs/public-api-audit.md, docs/api-stability.md),
// never an accident.
package compatibility_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// ---- compile-time signature pins (consumer surface) -----------------------

var (
	_ = client.New
	_ = client.ParsePhone
	_ = client.MarkedID
	_ = client.ResolveMarkedID
	_ = client.ClassifyError
	_ = client.AsConnectionLost
	_ = client.AsFloodWait

	_ = session.New
	_ = session.OpenFile
	_ = session.ParseStringSession

	_ client.PeerRef    = client.SelfRef{}
	_ client.PeerRef    = client.UsernameRef("")
	_ client.PeerRef    = client.PeerID{}
	_ client.MediaInput = (*client.UploadedPhotoInput)(nil)
	_ client.MediaInput = (*client.UploadedDocumentInput)(nil)
	_ client.MediaInput = (*client.ExistingPhotoInput)(nil)
	_ client.MediaInput = (*client.ExistingDocumentInput)(nil)
	_ client.Media      = (*client.Photo)(nil)
	_ client.Media      = (*client.Document)(nil)
	_ client.Media      = (*client.UnknownMedia)(nil)
)

// clientMethodShapes exercises every consumer-facing *Client method
// signature. Call shapes (not behavior) are the contract. It is pinned by
// the blank reference below — compile-time only, never executed.
var _ = clientMethodShapes

func clientMethodShapes(c *client.Client, ctx context.Context) { //nolint:unused
	var _ error = c.Connect(ctx)
	var _ error = c.Disconnect(ctx)
	var _ error = c.Close()
	var _ error = c.CatchUp()
	_, _ = c.AuthState(), c.State()
	_, _ = c.IsConnected(), c.Hooks()
	_, _ = c.Metrics(), c.EngineMetrics()
	_, _, _ = c.EngineMetricsDebug(), c.SessionKeyID(), c.LastError()
	_, _, _ = c.CachedEntities(), c.DroppedUpdates(), c.TrackedConversations()
	var _ = c.TransferMetrics()
	c.SetFloodWaitThreshold(0)

	var peer client.PeerRef = client.PeerID{Kind: client.PeerUser, ID: 1}
	var ids []int64
	_, _ = c.ResolvePeer(ctx, peer)
	_, _ = c.ResolvePeerString(ctx, "@x")
	_, _ = c.ResolveUsername(ctx, "@x")
	_, _ = c.ResolveReply(ctx, nil)
	_, _ = c.GetMe(ctx)
	_, _ = c.GetUser(ctx, peer)
	_, _ = c.GetChannel(ctx, peer)
	_, _ = c.GetChat(ctx, peer)
	_, _ = c.GetUserFull(ctx, peer)
	_, _ = c.GetChannelFull(ctx, peer)
	_, _ = c.GetChatFull(ctx, peer)
	_ = c.SetTyping(ctx, peer)

	var w io.Writer
	_, _ = c.SendMessage(ctx, nil, "m")
	_, _ = c.SendMessageOpts(ctx, peer, "m", client.SendMessageOptions{})
	_, _ = c.SendAlbum(ctx, peer, nil, client.SendAlbumOptions{})
	_, _ = c.SendMedia(ctx, nil, nil, client.UploadOptions{})
	_, _ = c.SendMediaInput(ctx, peer, nil)
	_, _ = c.EditMessage(ctx, peer, 1, "m")
	_, _ = c.DeleteMessages(ctx, peer, ids, client.DeleteMessagesOptions{})
	_, _ = c.ForwardMessages(ctx, peer, peer, ids, client.ForwardOptions{})
	_, _ = c.PinMessage(ctx, peer, 1, client.PinOptions{})
	_, _ = c.UnpinMessage(ctx, peer, 1)
	_ = c.MarkAsRead(ctx, peer, 1)
	_ = c.IsMessageRead(nil, 1)
	_ = c.IsOutgoingRead(nil, 1)
	_, _ = c.GetMessages(ctx, nil, client.MessageQuery{})
	_, _ = c.Reply(ctx, peer, 1, "m")
	_ = c.DownloadMedia(ctx, nil, w, client.DownloadOptions{})
	_, _ = c.Upload(ctx, nil, client.UploadOptions{})
	_ = c.IterDialogs(ctx, client.DialogQuery{})
	_, _ = c.IterMessages(ctx, peer, client.MessageQuery{})
	_, _ = c.IterSearchMessages(ctx, peer, client.SearchQuery{})
	_, _ = c.IterGlobalSearch(ctx, client.GlobalSearchQuery{})
	_, _ = c.SearchMessages(ctx, peer, client.SearchQuery{})
	_, _ = c.SearchGlobal(ctx, client.GlobalSearchQuery{})
	_, _ = c.Conversation(nil)
	_ = c.OnUpdate(func(ctx context.Context, upd tl.Object) {})
	_, _ = c.Invoke(ctx, &gen.PingRequest{PingID: 1})
	_, _ = c.SendCode(ctx, "phone")
	_, _ = c.SignIn(ctx, "phone", "code")
	_, _ = c.SignInWithHash(ctx, "phone", "code", "hash")
	_, _ = c.CheckPassword(ctx, "pw")
	_ = c.LogOut(ctx)
	_, _ = c.InputPeerFor(ctx, nil)
}

// ---- behavioral pins ------------------------------------------------------

// TestErrorSentinelsDistinct guards the sentinel inventory: every sentinel
// must be non-nil, distinct from every other, and matchable by errors.Is.
func TestErrorSentinelsDistinct(t *testing.T) {
	sentinels := []error{
		client.ErrClosed, client.ErrNotReady, client.ErrPhoneInvalid,
		client.ErrInvalidConfig, client.ErrInvalidPeer, client.ErrPeerNotFound,
		client.ErrReplyUnavailable, client.ErrInvalidMedia,
		client.ErrUnsupportedMedia, client.ErrMediaLocation,
		client.ErrFileReferenceExpired, client.ErrTransferClosed,
		client.ErrUpdateGap, client.ErrUpdateQueueFull,
		client.ErrSRPParams, client.ErrSRPAlgo, client.ErrBadState,
		rpc.ErrEngineClosed, rpc.ErrPendingLimit, rpc.ErrConnectionLost,
		session.ErrCorrupt, session.ErrNotAStringSession, session.ErrClosed,
		tl.ErrUnexpectedEOF,
	}
	for i, a := range sentinels {
		if a == nil {
			t.Fatalf("sentinel %d is nil", i)
		}
		if !errors.Is(a, a) {
			t.Fatalf("%v does not match itself", a)
		}
		for j := i + 1; j < len(sentinels); j++ {
			if errors.Is(a, sentinels[j]) {
				t.Fatalf("sentinels alias: %v == %v", a, sentinels[j])
			}
		}
	}
}

// TestClassifyErrorContract pins the coarse classes a caller may rely on.
func TestClassifyErrorContract(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"},
		{client.ErrClosed, "closed"},
		{client.ErrNotReady, "not_ready"},
		{&rpc.RPCError{Code: 400, Type: "PEER_ID_INVALID"}, "rpc_error"},
		{io.EOF, "unknown"},
	}
	for _, tc := range cases {
		if got := client.ClassifyError(tc.err); got != tc.want {
			t.Errorf("ClassifyError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestTypedErrorsAs pins errors.As support for the typed error families.
func TestTypedErrorsAs(t *testing.T) {
	e, ok := client.AsFloodWait(&rpc.RPCError{Code: 420, Type: "FLOOD_WAIT", Argument: 9})
	if !ok || e == nil || e.Seconds != 9 || e.Request != "FLOOD_WAIT" {
		t.Fatalf("AsFloodWait: %+v ok=%v", e, ok)
	}
	if _, ok := client.AsFloodWait(&rpc.RPCError{Code: 400, Type: "PEER_ID_INVALID"}); ok {
		t.Fatal("non-flood error matched AsFloodWait")
	}

	var connLost *client.ConnectionLostError
	if !errors.As(&client.ConnectionLostError{Err: io.EOF}, &connLost) {
		t.Fatal("ConnectionLostError not errors.As-able")
	}
	if client.AsConnectionLost(&client.ConnectionLostError{Err: io.EOF}) {
		t.Fatal("ConnectionLostError wrapper alone must not claim connection loss (needs rpc.ErrConnectionLost)")
	}

	var noHash *client.ErrAccessHashUnavailable
	if !errors.As(&client.ErrAccessHashUnavailable{Peer: client.PeerID{Kind: client.PeerUser, ID: 7}}, &noHash) {
		t.Fatal("ErrAccessHashUnavailable not errors.As-able")
	}
	if noHash.Peer.ID != 7 {
		t.Fatalf("peer id = %d", noHash.Peer.ID)
	}
}

// TestInternalTestSeamsPinned pins the documented INTERNAL test-seam
// methods: their signatures must not drift silently.
func TestInternalTestSeamsPinned(t *testing.T) {
	cfg := client.Config{
		Session: session.New(),
		Host:    "127.0.0.1:1",
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Skipf("client construction unavailable in this environment: %v", err)
	}
	defer c.Close()

	ah1, ah2 := int64(91), int64(92)
	c.SeedRaw(&gen.User{ID: 17, AccessHash: &ah1, UserType: &gen.UserTypeNormal{}})
	c.SeedEntities([]*client.User{{ID: 18, AccessHash: ah2}}, nil, nil)
	if c.CachedEntities() < 1 {
		t.Fatal("seeding did not populate the cache")
	}
	if c.ClosedForTest() {
		t.Fatal("fresh client reports closed")
	}
	c.ReconnectTransportForTest() // must not panic on a non-connected client
}

// TestLayerConstant pins the schema layer for the SDK contract.
func TestLayerConstant(t *testing.T) {
	if gen.Layer != 182 {
		t.Fatalf("Layer = %d, want 182", gen.Layer)
	}

}
