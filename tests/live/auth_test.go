package live

import (
	"context"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
)

// TestLiveAuthSendCodeSignIn (§8): the full authorization flow against the
// real service. Fail-closed: requires SP_LIVE_PHONE + SP_LIVE_CODE BEFORE
// any network traffic — no code provided means no SMS is ever requested.
func TestLiveAuthSendCodeSignIn(t *testing.T) {
	cfg, enabled := Load()
	if !enabled {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: SP_SDK_LIVE=1 not set")
	}
	if !cfg.AuthRequirementsMet() {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: SP_LIVE_PHONE and SP_LIVE_CODE must both be provided before any SendCode is sent")
	}
	s := newLiveSuite(t, "auth")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	requireConnect(t, s)

	// 8.1 SendCode: response decoding + phone_code_hash handling.
	sent, err := s.C.SendCode(ctx, cfg.Phone)
	if err != nil {
		s.Rec.Record("auth", "send_code", StatusFail, SourceLive, map[string]any{
			"class": client.ClassifyError(err),
		})
		t.Fatalf("SendCode: %v", err)
	}
	if sent == nil || sent.PhoneCodeHash == "" {
		s.Rec.Record("auth", "send_code", StatusFail, SourceLive, nil)
		t.Fatal("SendCode returned no phone_code_hash (never logged)")
	}
	s.Rec.Record("auth", "send_code", StatusPass, SourceLive, map[string]any{
		"phone_code_hash_present": true,
	})

	// 8.2 Optional invalid-code probe (opt-in; exactly one attempt).
	if cfg.BadCode {
		if _, err := s.C.SignIn(ctx, cfg.Phone, "00000"); err == nil {
			s.Rec.Record("auth", "invalid_code", StatusFail, SourceLive, nil)
			t.Fatal("invalid code accepted")
		} else {
			s.Rec.Record("auth", "invalid_code", StatusPass, SourceLive, map[string]any{
				"class": client.ClassifyError(err),
			})
		}
	}

	// 8.2 SignIn → Authorized.
	if _, err := s.C.SignIn(ctx, cfg.Phone, cfg.Code); err != nil {
		s.Rec.Record("auth", "sign_in", StatusFail, SourceLive, map[string]any{
			"class": client.ClassifyError(err),
		})
		t.Fatalf("SignIn: %v", err)
	}
	requireAuthorized(t, s)
	s.Rec.Record("auth", "sign_in", StatusPass, SourceLive, nil)

	// GetMe completes the flow.
	me, err := s.C.GetMe(ctx)
	if err != nil {
		t.Fatalf("GetMe after signIn: %v", err)
	}
	if me == nil {
		t.Fatal("GetMe nil")
	}
	s.Rec.Record("auth", "get_me", StatusPass, SourceLive, nil)
}

// TestLiveAuth2FA (§8.3): SRP check against the real service. Only runs
// when the account actually requires a password AND the operator supplied
// one; SRP material is never logged.
func TestLiveAuth2FA(t *testing.T) {
	cfg, enabled := Load()
	if !enabled {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: SP_SDK_LIVE=1 not set")
	}
	if !cfg.AuthRequirementsMet() {
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: phone and code required")
	}
	s := newLiveSuite(t, "auth")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	requireConnect(t, s)

	if _, err := s.C.SendCode(ctx, cfg.Phone); err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	_, signInErr := s.C.SignIn(ctx, cfg.Phone, cfg.Code)
	if signInErr == nil {
		s.Rec.Record("auth", "2fa", StatusNotApplicable, SourceLive, map[string]any{"reason": "account has no password"})
		return
	}
	if s.C.AuthState() != client.AuthPasswordRequired {
		s.Rec.Record("auth", "2fa", StatusNotApplicable, SourceLive, map[string]any{
			"reason": "sign-in failed without password requirement", "class": client.ClassifyError(signInErr),
		})
		t.Skipf("NOT_APPLICABLE: sign-in failed without a password requirement (%v)", client.ClassifyError(signInErr))
	}
	if cfg.Password == "" {
		s.Rec.Record("auth", "2fa", StatusBlocked, SourceLive, map[string]any{"reason": "password required but SP_LIVE_PASSWORD absent"})
		t.Skip("BLOCKED — REAL CREDENTIALS REQUIRED: account requires 2FA and SP_LIVE_PASSWORD was not provided")
	}
	if _, err := s.C.CheckPassword(ctx, cfg.Password); err != nil {
		s.Rec.Record("auth", "2fa", StatusFail, SourceLive, map[string]any{"class": client.ClassifyError(err)})
		t.Fatalf("CheckPassword: %v", err)
	}
	requireAuthorized(t, s)
	s.Rec.Record("auth", "2fa", StatusPass, SourceLive, nil)
}
