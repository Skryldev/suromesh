package live

import (
	"context"
	"testing"
	"time"
)

// TestLiveMediaRoundTrip (§20, §21 — P0): upload → send → download →
// SHA-256 equality across the reference's size ladder, streaming
// end-to-end (O(part) memory — the sink hashes; nothing materializes the
// file). Opt-in (SP_LIVE_MEDIA=1) because it writes to a real account.
func TestLiveMediaRoundTrip(t *testing.T) {
	s := newLiveSuite(t, "media")
	if !s.Cfg.Media {
		t.Skip("BLOCKED — SP_LIVE_MEDIA=1 not set (media transfers require explicit opt-in)")
	}
	if s.Cfg.Peer == "" {
		t.Skip("BLOCKED — SP_LIVE_PEER not configured (media needs a target peer)")
	}
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// §20 size ladder: minimal, small, part boundary, big-file threshold.
	for _, size := range []int64{1, 128 * 1024, 512 * 1024, 11 << 20} {
		if err := s.OpMediaRoundTrip(ctx, size); err != nil {
			s.Rec.Record("media", "round_trip", StatusFail, SourceLive, map[string]any{
				"size": size, "error": err.Error(),
			})
			t.Fatalf("media round trip (%d bytes): %v", size, err)
		}
	}
}
