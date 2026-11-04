package live

import (
	"context"
	"testing"
	"time"
)

// TestLiveIncomingExactlyOnce (§16): operator-assisted. The operator sends
// a message to the account from another session/app during the configured
// window; the harness verifies exactly-once subscriber delivery and
// MessageBox/Conversation coherence. Requires SP_LIVE_INCOMING_WAIT.
func TestLiveIncomingExactlyOnce(t *testing.T) {
	s := newLiveSuite(t, "updates")
	requireConnect(t, s)
	requireAuthorized(t, s)
	if s.Cfg.IncomingWait <= 0 {
		t.Skip("BLOCKED — SP_LIVE_INCOMING_WAIT not set (operator must send a message to the account during the window)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.Cfg.IncomingWait+2*time.Minute)
	defer cancel()

	if err := s.OpIncomingExactlyOnce(ctx, s.Cfg.IncomingWait); err != nil {
		t.Fatalf("incoming exactly-once: %v", err)
	}
}

// TestLiveGapAndDifference (§19): operator-assisted. Disconnect, operator
// sends during the gap, reconnect + difference must deliver exactly once.
// NOT_OBSERVED when no message arrives — never manufactured.
func TestLiveGapAndDifference(t *testing.T) {
	s := newLiveSuite(t, "updates")
	requireConnect(t, s)
	requireAuthorized(t, s)
	if s.Cfg.IncomingWait <= 0 {
		t.Skip("BLOCKED — SP_LIVE_INCOMING_WAIT not set (operator must send during the disconnect window)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.Cfg.IncomingWait+3*time.Minute)
	defer cancel()

	if err := s.OpGapAndDifference(ctx, 20*time.Second); err != nil {
		t.Fatalf("gap/difference: %v", err)
	}
}
