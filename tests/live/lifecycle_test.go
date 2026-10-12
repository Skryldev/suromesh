package live

// Phase 18 live-validation suites. Every test in this file family:
//   - skips (BLOCKED) unless SP_SDK_LIVE=1 AND the configuration it needs
//     is present (fail closed, §5);
//   - records sanitized evidence through the shared Recorder;
//   - classifies results LIVE / <status>.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
)

// newLiveSuite builds the shared suite for a live test in the given area,
// or reports BLOCKED via t.Skip when the harness is not enabled for that
// area. Guard rules (fail closed):
//   - SP_SDK_LIVE=1 required;
//   - the area must be named in SP_LIVE_AREAS;
//   - account areas require an authorized session file (SP_LIVE_SESSION) —
//     produced by a prior auth/session run with phone+code.
func newLiveSuite(t *testing.T, area string) *Suite {
	t.Helper()
	cfg, enabled := Load()
	if !enabled {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: SP_SDK_LIVE=1 not set (this is the expected outcome without operator credentials)")
	}
	if !cfg.AreaEnabled(area) {
		t.Skipf("BLOCKED — area %q not activated in SP_LIVE_AREAS (activation is explicit; nothing runs by accident)", area)
	}
	if area != "connect" && !cfg.AuthorizedSessionAvailable() {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: account areas need SP_LIVE_SESSION pointing at an authorized session file (run the auth area first)")
	}
	red := NewRedactor(cfg.SecretNames())
	rec, err := NewRecorder(cfg.EvidencePath, red)
	if err != nil {
		t.Fatalf("evidence recorder: %v", err)
	}
	trace := NewTrace(red)
	boot, err := Bootstrap(cfg, red, trace)
	if err != nil {
		rec.Close()
		t.Fatalf("bootstrap: %v", err)
	}
	s := &Suite{C: boot.Client, Trace: trace, Rec: rec, Cfg: cfg, Red: red}
	s.SessionPath = boot.SessionPath
	s.Addr = cfg.Host + ":" + itoa(cfg.Port)
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.Cleanup(ctx)
		_ = boot.Client.Close()
		rec.Record("lifecycle", "close", StatusPass, SourceLive, nil)
		CleanupSessionFile(boot.SessionPath, cfg.KeepArtifacts)
		CleanupRecorder(rec, cfg.KeepArtifacts)
	}
	t.Cleanup(stop)
	return s
}

// requireConnect connects the suite's client or fails the test.
func requireConnect(t *testing.T, s *Suite) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), liveConnectTimeout)
	defer cancel()
	if err := s.C.Connect(ctx); err != nil {
		s.Rec.Record("lifecycle", "connect", StatusFail, SourceLive, map[string]any{"error": client.ClassifyError(err)})
		t.Fatalf("connect: %v", err)
	}
	s.Rec.Record("lifecycle", "connect", StatusPass, SourceLive, map[string]any{
		"state": s.C.State().String(),
	})
}

// requireAuthorized asserts the Authorized state after connect.
func requireAuthorized(t *testing.T, s *Suite) {
	t.Helper()
	if s.C.AuthState() != client.AuthAuthorized {
		s.Rec.Record("auth", "state", StatusFail, SourceLive, map[string]any{"state": s.C.AuthState().String()})
		t.Fatalf("auth state = %s, want authorized", s.C.AuthState())
	}
}

// TestLiveLifecycleConnectClose validates §29 basics against the live
// service: connect → Ready, clean Close, zero pending.
func TestLiveLifecycleConnectClose(t *testing.T) {
	s := newLiveSuite(t, "connect")
	requireConnect(t, s)
	if s.C.State() != client.StateReady {
		t.Fatalf("state = %s, want ready", s.C.State())
	}
	if !s.C.IsConnected() {
		t.Fatal("IsConnected false in Ready")
	}
	if err := s.C.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s.C.State() != client.StateClosed {
		t.Fatalf("state after close = %s, want closed", s.C.State())
	}
	if _, err := s.C.Invoke(context.Background(), newProbePing()); !errors.Is(err, client.ErrClosed) {
		t.Fatalf("post-close invoke error = %v, want ErrClosed", err)
	}
	s.Rec.Record("lifecycle", "terminal_close", StatusPass, SourceLive, nil)
}
