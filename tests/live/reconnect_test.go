package live

import (
	"context"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
)

// TestLiveReconnect (§18): forced reconnect preserves authorization and
// the auth key, issues no authentication RPCs, and keeps the engine
// healthy (CatchUp succeeds).
func TestLiveReconnect(t *testing.T) {
	s := newLiveSuite(t, "reconnect")
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := s.OpReconnect(ctx); err != nil {
		s.Rec.Record("reconnect", "forced_reconnect", StatusFail, SourceLive, map[string]any{"error": err.Error()})
		t.Fatalf("reconnect: %v", err)
	}
}

// TestLiveReconnectGraceful (§18): the public Disconnect/Connect path on
// the live service — same invariants, graceful teardown.
func TestLiveReconnectGraceful(t *testing.T) {
	s := newLiveSuite(t, "reconnect")
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := s.C.Disconnect(ctx); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if err := s.C.Connect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !s.C.IsConnected() {
		t.Fatal("not connected after reconnect")
	}
	if s.C.AuthState() != client.AuthAuthorized {
		t.Fatalf("auth state after reconnect: %s", s.C.AuthState())
	}
	if s.Trace.HasAuthRPC() {
		t.Fatalf("graceful reconnect issued auth RPCs: %v", s.Trace.Methods())
	}
	s.Rec.Record("reconnect", "graceful", StatusPass, SourceLive, nil)
}
