package live

import (
	"context"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
)

// TestLivePeers (§10): GetMe, resolution, metadata refresh, and the
// never-fabricate invariant against the live service.
func TestLivePeers(t *testing.T) {
	s := newLiveSuite(t, "peers")
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// GetMe: server-provided identity.
	me, err := s.C.GetMe(ctx)
	if err != nil || me == nil {
		t.Fatalf("GetMe: %v", err)
	}
	s.Rec.Record("peers", "get_me", StatusPass, SourceLive, nil)

	if s.Cfg.Peer == "" {
		s.Rec.Record("peers", "resolution", StatusNotObserved, SourceLive, map[string]any{
			"reason": "SP_LIVE_PEER not configured",
		})
		t.Skip("NOT_OBSERVED: SP_LIVE_PEER not configured — resolution probes skipped")
	}

	// Resolve the configured peer by username.
	peer, err := s.C.ResolvePeer(ctx, s.Cfg.PeerRef())
	if err != nil {
		s.Rec.Record("peers", "resolve", StatusFail, SourceLive, map[string]any{"class": client.ClassifyError(err)})
		t.Fatalf("ResolvePeer: %v", err)
	}
	if peer == nil {
		t.Fatal("ResolvePeer returned nil")
	}
	s.Rec.Record("peers", "resolve", StatusPass, SourceLive, nil)

	// Metadata refresh (Phase 16 capability) over the resolved entity.
	refreshed, err := s.C.GetUser(ctx, s.Cfg.PeerRef())
	if err != nil {
		// The peer may be a chat/channel — try those families.
		if _, cerr := s.C.GetChat(ctx, s.Cfg.PeerRef()); cerr != nil {
			if _, cherr := s.C.GetChannel(ctx, s.Cfg.PeerRef()); cherr != nil {
				s.Rec.Record("peers", "metadata_refresh", StatusFail, SourceLive, map[string]any{"class": client.ClassifyError(err)})
				t.Fatalf("metadata refresh: %v", err)
			}
		}
	}
	if refreshed == nil {
		s.Rec.Record("peers", "metadata_refresh", StatusPass, SourceLive, map[string]any{"note": "chat/channel family"})
	} else {
		s.Rec.Record("peers", "metadata_refresh", StatusPass, SourceLive, nil)
	}

	// InputPeerFor: resolved entities must produce wire form without new
	// RPCs.
	if _, err := s.C.InputPeerFor(ctx, peer); err != nil {
		t.Fatalf("InputPeerFor: %v", err)
	}
	s.Rec.Record("peers", "input_peer", StatusPass, SourceLive, nil)
}
