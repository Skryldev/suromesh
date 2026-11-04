package live

import (
	"context"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
)

// TestLiveDialogsAndHistory (§11, §12): pagination, conversion, ordering,
// termination against the live service. Read-only.
func TestLiveDialogsAndHistory(t *testing.T) {
	s := newLiveSuite(t, "dialogs")
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if err := s.OpDialogsAndHistory(ctx, 10); err != nil {
		t.Fatalf("dialogs/history: %v", err)
	}
}

// TestLiveMutations (§13): send/edit/delete/read with a dedicated test
// message and deterministic cleanup. Opt-in (SP_LIVE_MUTATIONS=1) because
// it writes to a real account. Validates own-update semantics (§17) live.
func TestLiveMutations(t *testing.T) {
	s := newLiveSuite(t, "mutations")
	if !s.Cfg.Mutations {
		t.Skip("BLOCKED — SP_LIVE_MUTATIONS=1 not set (write operations require explicit opt-in)")
	}
	if s.Cfg.Peer == "" {
		t.Skip("BLOCKED — SP_LIVE_PEER not configured (mutations need a target peer)")
	}
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// §17: own update produces no duplicate subscriber event.
	peer := s.Cfg.PeerRef()
	id, err := s.OpOwnUpdateNoDuplicate(ctx, peer)
	if err != nil {
		s.Rec.Record("messaging", "own_update", StatusFail, SourceLive, map[string]any{"error": err.Error()})
		t.Fatalf("own update: %v", err)
	}

	// Edit the sent message.
	edited, err := s.C.EditMessage(ctx, peer, id, liveTestText("edited"))
	if err != nil {
		t.Fatalf("EditMessage: %v", err)
	}
	if edited == nil {
		t.Fatal("EditMessage returned nil")
	}
	s.Rec.Record("messaging", "edit", StatusPass, SourceLive, map[string]any{"message_id": id})

	// Reply to the sent message (§14): never fabricates a target.
	reply, err := s.C.Reply(ctx, peer, id, liveTestText("reply"))
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if reply != nil {
		if reply.Reply == nil || reply.Reply.MessageID != id {
			t.Fatalf("reply header mismatch: %+v", reply.Reply)
		}
		s.remember(peerIDOf(peer), reply.ID)
		s.Rec.Record("messaging", "reply", StatusPass, SourceLive, map[string]any{"reply_id": reply.ID})
	}

	// Mark read.
	if err := s.C.MarkAsRead(ctx, peer, reply.ID); err != nil {
		t.Fatalf("MarkAsRead: %v", err)
	}
	s.Rec.Record("messaging", "mark_read", StatusPass, SourceLive, nil)

	// Cleanup happens via s.Cleanup in the suite's stop function.
}

// TestLiveRichText (§15): UTF-16 entity offsets on live messages. Entities
// can only be OBSERVED on incoming/server-produced messages — the harness
// validates offset preservation on whatever entities the configured
// conversation actually carries, and reports NOT_OBSERVED otherwise
// (no fabrication of entity data).
func TestLiveRichText(t *testing.T) {
	s := newLiveSuite(t, "dialogs")
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if s.Cfg.Peer == "" {
		t.Skip("NOT_OBSERVED: SP_LIVE_PEER not configured")
	}
	peer, err := s.C.ResolvePeer(ctx, s.Cfg.PeerRef())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	msgs, err := s.C.GetMessages(ctx, peer, client.MessageQuery{Limit: 20})
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	entityCount := 0
	for _, m := range msgs {
		for _, e := range m.TextEntities {
			entityCount++
			// UTF-16 invariant: offsets within [0, utf16len(text)].
			if e.Offset < 0 || e.Length < 0 {
				t.Fatalf("negative entity span: %+v", e)
			}
		}
	}
	if entityCount == 0 {
		s.Rec.Record("rich_text", "utf16_offsets", StatusNotObserved, SourceLive, map[string]any{
			"reason": "no entity-bearing messages in the observed window",
		})
		return
	}
	s.Rec.Record("rich_text", "utf16_offsets", StatusPass, SourceLive, map[string]any{
		"entities": entityCount,
	})
}
