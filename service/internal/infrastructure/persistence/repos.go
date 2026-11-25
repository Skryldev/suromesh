package persistence

// Per-port repository adapters over one shared pool. Each port gets its
// own type (no name collisions across interface sets), each with
// compile-time conformance assertions.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// Store bundles the pool and exposes the per-port adapters.
type Store struct {
	pool        *pgxpool.Pool
	Tenants     *TenantRepo
	Credentials *CredentialRepo
	Accounts    *AccountRepo
	Sessions    *SessionRepo
	Operations  *OperationRepo
	Audit       *AuditRepo
}

// NewStore builds the repository bundle.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{
		pool:        pool,
		Tenants:     &TenantRepo{pool: pool},
		Credentials: &CredentialRepo{pool: pool},
		Accounts:    &AccountRepo{pool: pool},
		Sessions:    &SessionRepo{pool: pool},
		Operations:  &OperationRepo{pool: pool},
		Audit:       &AuditRepo{pool: pool},
	}
}

// Pool exposes the underlying pool for health checks (readiness).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// ---- shared helpers ---------------------------------------------------------

func parseAccountStatus(s string) domain.AccountStatus {
	switch s {
	case "pending_auth":
		return domain.AccountStatusPendingAuth
	case "active":
		return domain.AccountStatusActive
	case "suspended":
		return domain.AccountStatusSuspended
	case "removed":
		return domain.AccountStatusRemoved
	default:
		return domain.AccountStatusUnspecified
	}
}

func parseDesired(s string) domain.DesiredState {
	if s == "running" {
		return domain.DesiredRunning
	}
	return domain.DesiredStopped
}

func statusValue(s domain.AccountStatus) string {
	switch s {
	case domain.AccountStatusPendingAuth:
		return "pending_auth"
	case domain.AccountStatusActive:
		return "active"
	case domain.AccountStatusSuspended:
		return "suspended"
	case domain.AccountStatusRemoved:
		return "removed"
	default:
		return "pending_auth"
	}
}

func desiredValue(s domain.DesiredState) string {
	if s == domain.DesiredRunning {
		return "running"
	}
	return "stopped"
}

const accountCols = "id, tenant_id, display_alias, phone_hash, phone_mask, status, desired, created_at"

func scanAccount(row pgx.Row) (ports.AccountRecord, error) {
	var rec ports.AccountRecord
	var status, desired string
	err := row.Scan(&rec.AccountID, &rec.TenantID, &rec.DisplayAlias, &rec.PhoneHash,
		&rec.PhoneMask, &status, &desired, &rec.CreatedAt)
	if err != nil {
		return ports.AccountRecord{}, err
	}
	rec.Status = parseAccountStatus(status)
	rec.Desired = parseDesired(desired)
	return rec, nil
}

// ---- tenants ------------------------------------------------------------------

// TenantRepo implements ports.TenantRepository.
type TenantRepo struct{ pool *pgxpool.Pool }

var _ ports.TenantRepository = (*TenantRepo)(nil)

func (r *TenantRepo) Create(ctx context.Context, name string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ($1) RETURNING id`, name).Scan(&id)
	if err != nil {
		return "", mapPgxErr(err, "tenant")
	}
	return id, nil
}

// GetOrCreate resolves a tenant by name, creating it when absent. The
// concurrent-insert case is settled by the name UNIQUE constraint: the
// loser reads the winner's row.
func (r *TenantRepo) GetOrCreate(ctx context.Context, name string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM tenants WHERE name = $1`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", mapPgxErr(err, "tenant lookup")
	}
	if err := r.pool.QueryRow(ctx,
		`INSERT INTO tenants (name) VALUES ($1)
		 ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		 RETURNING id`, name).Scan(&id); err != nil {
		return "", mapPgxErr(err, "tenant create")
	}
	return id, nil
}

func (r *TenantRepo) Get(ctx context.Context, id string) (ports.TenantRecord, error) {
	var rec ports.TenantRecord
	err := r.pool.QueryRow(ctx,
		`SELECT id, name, created_at FROM tenants WHERE id = $1`, id).
		Scan(&rec.ID, &rec.Name, &rec.CreatedAt)
	if err != nil {
		return ports.TenantRecord{}, mapPgxErr(err, "tenant")
	}
	return rec, nil
}

// ---- credentials ---------------------------------------------------------------

// CredentialRepo implements ports.CredentialStore.
type CredentialRepo struct{ pool *pgxpool.Pool }

var _ ports.CredentialStore = (*CredentialRepo)(nil)

func (r *CredentialRepo) LookupByHashPrefix(ctx context.Context, prefix string) ([]ports.CredentialRecord, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, tenant_id, key_hash, prefix, permissions, status
		FROM credentials WHERE prefix = $1 AND status = 'active'`, prefix)
	if err != nil {
		return nil, mapPgxErr(err, "credentials")
	}
	defer rows.Close()
	var out []ports.CredentialRecord
	for rows.Next() {
		var rec ports.CredentialRecord
		if err := rows.Scan(&rec.ID, &rec.TenantID, &rec.KeyHash, &rec.Prefix,
			&rec.Permissions, &rec.Status); err != nil {
			return nil, mapPgxErr(err, "credential row")
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CreateCredential inserts one credential row (hash + prefix only).
func (r *CredentialRepo) CreateCredential(ctx context.Context, tenantID, keyHash, keyPrefix string, perms []string) (string, error) {
	var id string
	err := r.pool.QueryRow(ctx, `
		INSERT INTO credentials (tenant_id, key_hash, prefix, permissions)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		tenantID, keyHash, keyPrefix, perms).Scan(&id)
	if err != nil {
		return "", mapPgxErr(err, "credential")
	}
	return id, nil
}

// RevokeCredential flips the credential status (immediate).
func (r *CredentialRepo) RevokeCredential(ctx context.Context, tenantID, credentialID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE credentials SET status = 'revoked', revoked_at = now()
		WHERE id = $1 AND tenant_id = $2`, credentialID, tenantID)
	return mapPgxErr(err, "credential revoke")
}

// ---- accounts -------------------------------------------------------------------

// AccountRepo implements ports.AccountRepository.
type AccountRepo struct{ pool *pgxpool.Pool }

var _ ports.AccountRepository = (*AccountRepo)(nil)

func (r *AccountRepo) Create(ctx context.Context, a ports.AccountRecord) (ports.AccountRecord, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO accounts (tenant_id, display_alias, phone_hash, phone_mask, status, desired)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+accountCols,
		a.TenantID, a.DisplayAlias, a.PhoneHash, a.PhoneMask,
		statusValue(a.Status), desiredValue(a.Desired))
	rec, err := scanAccount(row)
	if err != nil {
		return ports.AccountRecord{}, mapPgxErr(err, "account")
	}
	return rec, nil
}

func (r *AccountRepo) GetByID(ctx context.Context, tenantID, accountID string) (ports.AccountRecord, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+accountCols+` FROM accounts WHERE id = $1::uuid AND tenant_id = $2::uuid`,
		accountID, tenantID)
	rec, err := scanAccount(row)
	if err != nil {
		return ports.AccountRecord{}, mapPgxErr(err, "account")
	}
	return rec, nil
}

func (r *AccountRepo) GetByPhoneHash(ctx context.Context, tenantID, phoneHash string) (ports.AccountRecord, error) {
	row := r.pool.QueryRow(ctx,
		`SELECT `+accountCols+` FROM accounts WHERE phone_hash = $1 AND tenant_id = $2::uuid`,
		phoneHash, tenantID)
	rec, err := scanAccount(row)
	if err != nil {
		return ports.AccountRecord{}, mapPgxErr(err, "account")
	}
	return rec, nil
}

func (r *AccountRepo) List(ctx context.Context, tenantID string, limit, offset int) ([]ports.AccountRecord, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+accountCols+` FROM accounts WHERE tenant_id = $1::uuid
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`, tenantID, limit, offset)
	if err != nil {
		return nil, mapPgxErr(err, "accounts")
	}
	defer rows.Close()
	var out []ports.AccountRecord
	for rows.Next() {
		rec, err := scanAccount(rows)
		if err != nil {
			return nil, mapPgxErr(err, "account row")
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *AccountRepo) SetStatus(ctx context.Context, tenantID, accountID string, status domain.AccountStatus) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE accounts SET status = $1 WHERE id = $2::uuid AND tenant_id = $3::uuid`,
		statusValue(status), accountID, tenantID)
	return mapPgxErr(err, "account status")
}

func (r *AccountRepo) SetDesiredState(ctx context.Context, tenantID, accountID string, d domain.DesiredState) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE accounts SET desired = $1 WHERE id = $2::uuid AND tenant_id = $3::uuid`,
		desiredValue(d), accountID, tenantID)
	return mapPgxErr(err, "account desired")
}

func (r *AccountRepo) Delete(ctx context.Context, tenantID, accountID string) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM accounts WHERE id = $1::uuid AND tenant_id = $2::uuid`, accountID, tenantID)
	return mapPgxErr(err, "account delete")
}

// ---- sessions ---------------------------------------------------------------------

// SessionRepo implements ports.SessionRepository (encrypted blobs only).
type SessionRepo struct{ pool *pgxpool.Pool }

var _ ports.SessionRepository = (*SessionRepo)(nil)

func (r *SessionRepo) Store(ctx context.Context, account domain.AccountID, blob []byte) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO sessions (account_id, blob, updated_at)
		VALUES ($1::uuid, $2, now())
		ON CONFLICT (account_id) DO UPDATE
		SET blob = EXCLUDED.blob, updated_at = now()`, string(account), blob)
	return mapPgxErr(err, "session store")
}

func (r *SessionRepo) Load(ctx context.Context, account domain.AccountID) ([]byte, error) {
	var blob []byte
	err := r.pool.QueryRow(ctx,
		`SELECT blob FROM sessions WHERE account_id = $1::uuid`, string(account)).Scan(&blob)
	if err != nil {
		return nil, mapPgxErr(err, "session load")
	}
	return blob, nil
}

// Delete crypto-shreds the row (the wrapped DEK dies with it).
func (r *SessionRepo) Delete(ctx context.Context, account domain.AccountID) error {
	_, err := r.pool.Exec(ctx,
		`DELETE FROM sessions WHERE account_id = $1::uuid`, string(account))
	return mapPgxErr(err, "session delete")
}

// ---- operations (idempotency) -------------------------------------------------------

// OperationRepo implements ports.OperationRepository.
type OperationRepo struct{ pool *pgxpool.Pool }

var _ ports.OperationRepository = (*OperationRepo)(nil)

// Claim atomically claims an idempotency slot. Existing records return
// (record, false); key reuse with a different fingerprint is a conflict.
func (r *OperationRepo) Claim(ctx context.Context, rec ports.OperationRecord) (ports.OperationRecord, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ports.OperationRecord{}, false, mapPgxErr(err, "op claim begin")
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existing ports.OperationRecord
	var state string
	// result_ref is nullable (NULL until Finish): scan via *string.
	var resultRef *string
	err = tx.QueryRow(ctx, `
		SELECT id, state, result_ref, fingerprint FROM operations
		WHERE tenant_id = $1::uuid AND kind = $2 AND op_key = $3
		FOR UPDATE`, rec.TenantID, rec.Kind, rec.Key).
		Scan(&existing.ID, &state, &resultRef, &existing.Fingerprint)
	switch {
	case err == nil:
		if rec.Fingerprint != "" && existing.Fingerprint != "" &&
			existing.Fingerprint != rec.Fingerprint {
			return ports.OperationRecord{}, false, fmt.Errorf(
				"%w: idempotency key reused with different request", domain.ErrAlreadyExists)
		}
		existing.State = state
		if resultRef != nil {
			existing.ResultRef = *resultRef
		}
		return existing, false, tx.Commit(ctx)
	case errors.Is(err, pgx.ErrNoRows):
		// fall through to insert
	default:
		return ports.OperationRecord{}, false, mapPgxErr(err, "op claim select")
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO operations (tenant_id, account_id, kind, op_key, fingerprint, state)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'claimed')
		ON CONFLICT (tenant_id, kind, op_key) DO NOTHING
		RETURNING id`, rec.TenantID, rec.AccountID, rec.Kind, rec.Key, rec.Fingerprint).
		Scan(&rec.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.OperationRecord{}, false, fmt.Errorf(
			"%w: operation claim raced", domain.ErrAlreadyExists)
	}
	if err != nil {
		return ports.OperationRecord{}, false, mapPgxErr(err, "op claim insert")
	}
	rec.State = "claimed"
	return rec, true, tx.Commit(ctx)
}

// Finish records the terminal outcome.
func (r *OperationRepo) Finish(ctx context.Context, tenantID, opID, state, resultRef string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE operations SET state = $1, result_ref = NULLIF($2, ''), updated_at = now()
		WHERE id = $3::uuid AND tenant_id = $4::uuid`, state, resultRef, opID, tenantID)
	return mapPgxErr(err, "operation finish")
}

// ---- audit ---------------------------------------------------------------------------

// AuditRepo implements ports.AuditRecorder.
type AuditRepo struct{ pool *pgxpool.Pool }

var _ ports.AuditRecorder = (*AuditRepo)(nil)

func (r *AuditRepo) Record(ctx context.Context, e ports.AuditEvent) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO audit_events (actor, tenant_id, target, action, outcome, detail)
		VALUES ($1, NULLIF($2, '')::uuid, $3, $4, $5, $6)`,
		e.Actor, e.TenantID, e.Target, e.Action, e.Outcome, e.Detail)
	return mapPgxErr(err, "audit")
}

// Compile-time port conformance (drift fails the build).
var (
	_ ports.TenantRepository    = (*TenantRepo)(nil)
	_ ports.CredentialStore     = (*CredentialRepo)(nil)
	_ ports.AccountRepository   = (*AccountRepo)(nil)
	_ ports.SessionRepository   = (*SessionRepo)(nil)
	_ ports.OperationRepository = (*OperationRepo)(nil)
	_ ports.AuditRecorder       = (*AuditRepo)(nil)
)
