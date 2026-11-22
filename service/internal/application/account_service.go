package application

// AccountService: the account-binding use cases. Tenant scoping is
// enforced here (the transport passes the authenticated principal's
// tenant), never by trusting caller-supplied tenant ids.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// phoneNormalize mirrors the reference parse_phone: strip '+', '(',
// ')', '-', spaces; remainder must be non-empty ASCII digits.
var phoneStrip = regexp.MustCompile(`[+()\-\s]`)

func normalizePhone(phone string) (string, error) {
	cleaned := phoneStrip.ReplaceAllString(phone, "")
	if cleaned == "" {
		return "", fmt.Errorf("%w: empty phone", domain.ErrInvalidInput)
	}
	for _, r := range cleaned {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: phone contains non-digit %q", domain.ErrInvalidInput, r)
		}
	}
	if len(cleaned) < 6 || len(cleaned) > 15 {
		return "", fmt.Errorf("%w: phone length %d out of range", domain.ErrInvalidInput, len(cleaned))
	}
	return cleaned, nil
}

// MaskPhone renders the display-only mask ("+98 *** ** 21 77" shape).
func MaskPhone(normalized string) string {
	if len(normalized) < 6 {
		return "***"
	}
	cc := normalized[:2]
	tail := normalized[len(normalized)-4:]
	mid := strings.Repeat("•", len(normalized)-6)
	return "+" + cc + " " + mid + " " + tail
}

// PhoneHashKey is the HMAC key for phone lookup hashes (from config; a
// plain SHA-256 would allow offline enumeration given DB access).
type PhoneHashKey struct{ Key []byte }

func (k PhoneHashKey) hash(normalized string) string {
	mac := hmac.New(sha256.New, k.Key)
	mac.Write([]byte(normalized))
	return hex.EncodeToString(mac.Sum(nil))
}

// AccountService implements the binding use cases over the repositories.
type AccountService struct {
	accounts ports.AccountRepository
	sessions ports.SessionRepository
	audit    ports.AuditRecorder
	phoneKey PhoneHashKey
	manager  *Manager
}

// audit is the sanitized audit helper (never payloads).
func (s *AccountService) auditEvent(ctx context.Context, actor, tenantID, target, actionOutcome string) {
	if s.audit == nil {
		return
	}
	outcome := "success"
	action := actionOutcome
	if i := strings.LastIndex(actionOutcome, " "); i > 0 {
		action = actionOutcome[:i]
		outcome = actionOutcome[i+1:]
	}
	_ = s.audit.Record(ctx, ports.AuditEvent{
		Time: time.Now(), Actor: actor, TenantID: tenantID,
		Target: target, Action: action, Outcome: outcome,
	})
}

// NewAccountService wires the account use cases.
func NewAccountService(accounts ports.AccountRepository, sessions ports.SessionRepository,
	audit ports.AuditRecorder, phoneKey PhoneHashKey, manager *Manager) *AccountService {
	return &AccountService{
		accounts: accounts, sessions: sessions, audit: audit,
		phoneKey: phoneKey, manager: manager,
	}
}

// BoundAccount is the use-case result (already masked; no raw phone).
type BoundAccount struct {
	Record   ports.AccountRecord
	Existing bool
}

// Bind binds a phone to the tenant (idempotent per phone within the
// tenant). The runner is NOT started here: authentication must complete
// first (Connect is meaningful once session material exists).
func (s *AccountService) Bind(ctx context.Context, tenantID, actor, phone, alias string) (BoundAccount, error) {
	normalized, err := normalizePhone(phone)
	if err != nil {
		s.auditEvent(ctx, actor, tenantID, phone, "account.bind failure")
		return BoundAccount{}, err
	}
	if strings.TrimSpace(alias) == "" || len(alias) > 64 {
		s.auditEvent(ctx, actor, tenantID, phone, "account.bind failure")
		return BoundAccount{}, fmt.Errorf("%w: display_alias required (1..64 chars)", domain.ErrInvalidInput)
	}
	phoneHash := s.phoneKey.hash(normalized)

	if existing, err := s.accounts.GetByPhoneHash(ctx, tenantID, phoneHash); err == nil {
		// Idempotent rebind within the tenant.
		return BoundAccount{Record: existing, Existing: true}, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return BoundAccount{}, err
	}

	rec, err := s.accounts.Create(ctx, ports.AccountRecord{
		TenantID:     tenantID,
		DisplayAlias: alias,
		PhoneHash:    phoneHash,
		PhoneMask:    MaskPhone(normalized),
		Status:       domain.AccountStatusPendingAuth,
		Desired:      domain.DesiredStopped,
	})
	if err != nil {
		s.auditEvent(ctx, actor, tenantID, phone, "account.bind failure")
		return BoundAccount{}, err
	}
	s.auditEvent(ctx, actor, tenantID, rec.AccountID, "account.bind success")
	return BoundAccount{Record: rec}, nil
}

// Get resolves one account within the tenant (NOT_FOUND cross-tenant).
func (s *AccountService) Get(ctx context.Context, tenantID, accountID string) (ports.AccountRecord, error) {
	return s.accounts.GetByID(ctx, tenantID, accountID)
}

// List pages the tenant's accounts.
func (s *AccountService) List(ctx context.Context, tenantID string, limit, offset int) ([]ports.AccountRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	return s.accounts.List(ctx, tenantID, limit, offset)
}

// Suspend stops the runtime and flips status.
func (s *AccountService) Suspend(ctx context.Context, tenantID, actor, accountID string) (ports.AccountRecord, error) {
	rec, err := s.accounts.GetByID(ctx, tenantID, accountID)
	if err != nil {
		return ports.AccountRecord{}, err
	}
	if err := s.manager.StopRunner(ctx, tenantID, domain.AccountID(rec.AccountID)); err != nil &&
		!errors.Is(err, domain.ErrNotFound) {
		return ports.AccountRecord{}, err
	}
	if err := s.accounts.SetStatus(ctx, tenantID, accountID, domain.AccountStatusSuspended); err != nil {
		return ports.AccountRecord{}, err
	}
	s.auditEvent(ctx, actor, tenantID, accountID, "account.suspend success")
	return s.accounts.GetByID(ctx, tenantID, accountID)
}

// Resume flips status back (connect follows via ConnectionService).
func (s *AccountService) Resume(ctx context.Context, tenantID, actor, accountID string) (ports.AccountRecord, error) {
	if err := s.accounts.SetStatus(ctx, tenantID, accountID, domain.AccountStatusActive); err != nil {
		return ports.AccountRecord{}, err
	}
	s.auditEvent(ctx, actor, tenantID, accountID, "account.resume success")
	return s.accounts.GetByID(ctx, tenantID, accountID)
}

// Remove stops the runtime and crypto-shreds the session material.
func (s *AccountService) Remove(ctx context.Context, tenantID, actor, accountID string) error {
	rec, err := s.accounts.GetByID(ctx, tenantID, accountID)
	if err != nil {
		return err
	}
	if err := s.manager.StopRunner(ctx, tenantID, domain.AccountID(rec.AccountID)); err != nil &&
		!errors.Is(err, domain.ErrNotFound) {
		return err
	}
	// Crypto-shred: the session row (blob + wrapped DEK) is deleted.
	if err := s.sessions.Delete(ctx, domain.AccountID(rec.AccountID)); err != nil &&
		!errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err := s.accounts.Delete(ctx, tenantID, accountID); err != nil {
		return err
	}
	s.auditEvent(ctx, actor, tenantID, accountID, "account.remove success")
	return nil
}
