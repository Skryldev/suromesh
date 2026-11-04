package live

import (
	"context"
	"testing"
	"time"
)

// TestLiveSessionReuse (§9 — critical): a persisted, authorized session
// restored into a new client MUST reconnect and serve GetMe WITHOUT any
// authentication RPC. Two iterations prove it is not a one-off.
func TestLiveSessionReuse(t *testing.T) {
	cfg, enabled := Load()
	if !enabled {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: SP_SDK_LIVE=1 not set")
	}
	if !cfg.AuthRequirementsMet() {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: phone + code required to establish the baseline authorization")
	}
	s := newLiveSuite(t, "session")

	// Baseline: authorize once and persist into the suite's session file.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	requireConnect(t, s)
	if _, err := s.C.SendCode(ctx, cfg.Phone); err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	if _, err := s.C.SignIn(ctx, cfg.Phone, cfg.Code); err != nil {
		t.Fatalf("SignIn: %v", err)
	}
	requireAuthorized(t, s)
	if err := s.C.Close(); err != nil {
		t.Fatalf("close after authorize: %v", err)
	}
	// Trace reset: from here on, ANY auth RPC is a violation.
	s.Trace = NewTrace(s.Red)

	if err := s.OpSessionReuse(ctx, s.sessionFactoryFromPath(), 2); err != nil {
		s.Rec.Record("session", "reuse", StatusFail, SourceLive, map[string]any{"error": err.Error()})
		t.Fatalf("session reuse: %v", err)
	}
}
