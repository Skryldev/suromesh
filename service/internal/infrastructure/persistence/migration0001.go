package persistence

// migration0001 holds the initial schema (kept in Go so the binary is
// self-contained; the identical SQL lives in migrations/0001_init.sql for
// review and external psql application).
const migration0001 = `
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS tenants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS credentials (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_hash    CHAR(64) NOT NULL UNIQUE,
    prefix      CHAR(8)  NOT NULL,
    permissions TEXT[] NOT NULL DEFAULT '{}',
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK (status IN ('active','revoked')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_credentials_prefix ON credentials(prefix) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS idx_credentials_tenant ON credentials(tenant_id);

CREATE TABLE IF NOT EXISTS accounts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    display_alias TEXT NOT NULL,
    phone_hash    CHAR(64) NOT NULL,
    phone_mask    TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending_auth'
                  CHECK (status IN ('pending_auth','active','suspended','removed')),
    desired       TEXT NOT NULL DEFAULT 'stopped'
                  CHECK (desired IN ('stopped','running')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, phone_hash)
);
CREATE INDEX IF NOT EXISTS idx_accounts_tenant ON accounts(tenant_id, status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_phone_hash ON accounts(phone_hash);

CREATE TABLE IF NOT EXISTS sessions (
    account_id     UUID PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    blob           BYTEA NOT NULL,
    format_version TEXT NOT NULL DEFAULT 'v1',
    kek_version    TEXT NOT NULL DEFAULT 'dev-1',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS operations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    account_id   UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,
    op_key       TEXT NOT NULL,
    fingerprint  CHAR(64) NOT NULL,
    state        TEXT NOT NULL DEFAULT 'created'
                 CHECK (state IN ('created','claimed','sent','completed','failed','cancelled','timed_out','ambiguous')),
    result_ref   TEXT,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, kind, op_key)
);
CREATE INDEX IF NOT EXISTS idx_operations_account ON operations(account_id, updated_at);

CREATE TABLE IF NOT EXISTS audit_events (
    id         BIGSERIAL PRIMARY KEY,
    time       TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor      TEXT NOT NULL,
    tenant_id  UUID,
    target     TEXT NOT NULL,
    action     TEXT NOT NULL,
    outcome    TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_events(time DESC);
`
