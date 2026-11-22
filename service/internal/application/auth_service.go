package application

// AuthService: authentication orchestration. The OTP and 2FA password
// pass through these methods exactly once and are never stored: they
// travel to the adapter call and vanish.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// AuthService implements the authentication use cases over the account
// runtime.
type AuthService struct {
	manager  *Manager
	accounts ports.AccountRepository
	audit    ports.AuditRecorder
	phoneKey PhoneHashKey
}

// NewAuthService wires the auth use cases.
func NewAuthService(manager *Manager, accounts ports.AccountRepository,
	audit ports.AuditRecorder, phoneKey PhoneHashKey) *AuthService {
	return &AuthService{manager: manager, accounts: accounts, audit: audit, phoneKey: phoneKey}
}

// ensureRunner returns the account's runner, starting one if the account
// binding exists and is not suspended (read path for auth flows: an
// account mid-authentication has no live runtime yet).
func (s *AuthService) EnsureRunner(ctx context.Context, tenantID, accountID string) (*Runner, error) {
	rec, err := s.accounts.GetByID(ctx, tenantID, accountID)
	if err != nil {
		return nil, err
	}
	if rec.Status == domain.AccountStatusSuspended || rec.Status == domain.AccountStatusRemoved {
		return nil, fmt.Errorf("%w: account %s", domain.ErrAccountStopped, accountID)
	}
	runner, err := s.manager.Get(tenantID, domain.AccountID(rec.AccountID))
	if errors.Is(err, domain.ErrNotFound) {
		runner, err = s.manager.StartRunner(ctx, RunnerConfig{
			AccountID: domain.AccountID(rec.AccountID),
			TenantID:  tenantID,
		})
		if err != nil {
			return nil, err
		}
	}
	return runner, err
}

// SendCode requests an upstream authentication code (exactly one call —
// never retried by the service; flood waits surface to the caller). The
// phone is verified against the binding's lookup hash before any
// upstream traffic.
func (s *AuthService) SendCode(ctx context.Context, tenantID, actor, accountID, phone string) (domain.AuthState, error) {
	normalized, err := normalizePhone(phone)
	if err != nil {
		return domain.AuthUnauthorized, err
	}
	runner, err := s.EnsureRunner(ctx, tenantID, accountID)
	if err != nil {
		return domain.AuthUnauthorized, err
	}
	if err := verifyPhone(ctx, s.accounts, tenantID, accountID, s.phoneKey, normalized); err != nil {
		return domain.AuthUnauthorized, err
	}
	err = runner.Mutate(ctx, func(sess ports.ClientSession) error {
		return sess.SendCode(ctx, phone)
	})
	if err != nil {
		s.auditEvent(ctx, actor, tenantID, accountID, "auth.code_requested failure")
		return domain.AuthUnauthorized, err
	}
	s.auditEvent(ctx, actor, tenantID, accountID, "auth.code_requested success")
	return runner.SessionAuthState(), nil
}

// SubmitCode completes code verification.
func (s *AuthService) SubmitCode(ctx context.Context, tenantID, actor, accountID, phone, code string) (domain.AuthState, error) {
	normalized, err := normalizePhone(phone)
	if err != nil {
		return domain.AuthUnauthorized, err
	}
	runner, err := s.EnsureRunner(ctx, tenantID, accountID)
	if err != nil {
		return domain.AuthUnauthorized, err
	}
	if err := verifyPhone(ctx, s.accounts, tenantID, accountID, s.phoneKey, normalized); err != nil {
		return domain.AuthUnauthorized, err
	}
	err = runner.Mutate(ctx, func(sess ports.ClientSession) error {
		return sess.SubmitCode(ctx, phone, code)
	})
	if err != nil {
		s.auditEvent(ctx, actor, tenantID, accountID, "auth.submit_code failure")
		return domain.AuthUnauthorized, err
	}
	s.auditEvent(ctx, actor, tenantID, accountID, "auth.submit_code success")
	state := runner.SessionAuthState()
	if state == domain.AuthAuthorized {
		if err := s.accounts.SetStatus(ctx, tenantID, accountID, domain.AccountStatusActive); err != nil {
			return state, err
		}
	}
	return state, nil
}

// SubmitPassword completes 2FA.
func (s *AuthService) SubmitPassword(ctx context.Context, tenantID, actor, accountID, password string) (domain.AuthState, error) {
	runner, err := s.EnsureRunner(ctx, tenantID, accountID)
	if err != nil {
		return domain.AuthUnauthorized, err
	}
	err = runner.Mutate(ctx, func(sess ports.ClientSession) error {
		return sess.SubmitPassword(ctx, password)
	})
	if err != nil {
		s.auditEvent(ctx, actor, tenantID, accountID, "auth.submit_password failure")
		return domain.AuthPasswordRequired, err
	}
	s.auditEvent(ctx, actor, tenantID, accountID, "auth.submit_password success")
	state := runner.SessionAuthState()
	if state == domain.AuthAuthorized {
		if err := s.accounts.SetStatus(ctx, tenantID, accountID, domain.AccountStatusActive); err != nil {
			return state, err
		}
	}
	return state, nil
}

// LogOut invalidates the session upstream and locally (crypto-shred).
func (s *AuthService) LogOut(ctx context.Context, tenantID, actor, accountID string) error {
	runner, err := s.EnsureRunner(ctx, tenantID, accountID)
	if err != nil {
		return err
	}
	if err := runner.Mutate(ctx, func(sess ports.ClientSession) error {
		return sess.LogOut(ctx)
	}); err != nil {
		return err
	}
	if err := s.accounts.SetStatus(ctx, tenantID, accountID, domain.AccountStatusPendingAuth); err != nil {
		return err
	}
	s.auditEvent(ctx, actor, tenantID, accountID, "auth.logout success")
	return nil
}

// auditEvent mirrors AccountService's helper (shared shape).
func (s *AuthService) auditEvent(ctx context.Context, actor, tenantID, target, actionOutcome string) {
	if s.audit == nil {
		return
	}
	outcome := "success"
	action := actionOutcome
	if i := len(actionOutcome) - len(" failure"); i > 0 && actionOutcome[i:] == " failure" {
		action = actionOutcome[:i]
		outcome = "failure"
	}
	_ = s.audit.Record(ctx, ports.AuditEvent{
		Time: timeNow(), Actor: actor, TenantID: tenantID,
		Target: target, Action: action, Outcome: outcome,
	})
}

func timeNow() time.Time { return time.Now() }

// AuthorizationState resolves the current auth state (starts a runner
// when none exists yet — an account mid-authentication has no live
// runtime).
func (s *AuthService) AuthorizationState(ctx context.Context, tenantID, accountID string) (domain.AuthState, error) {
	runner, err := s.EnsureRunner(ctx, tenantID, accountID)
	if err != nil {
		return domain.AuthUnauthorized, err
	}
	return runner.SessionAuthState(), nil
}

// verifyPhone checks the caller-supplied phone against the binding's
// stored lookup hash (the raw phone is never stored; mismatch →
// ErrInvalidInput with no detail about the bound phone).
func verifyPhone(ctx context.Context, accounts ports.AccountRepository,
	tenantID, accountID string, key PhoneHashKey, normalized string) error {
	rec, err := accounts.GetByID(ctx, tenantID, accountID)
	if err != nil {
		return err
	}
	if rec.PhoneHash != key.hash(normalized) {
		return fmt.Errorf("%w: phone does not match the bound account",
			domain.ErrInvalidInput)
	}
	return nil
}
