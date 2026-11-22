// Package ports defines the driven-port interfaces of the hexagonal
// architecture. The application layer depends on these; adapters in
// internal/infrastructure implement them.
//
// BOUNDARY: interfaces reference domain types only. The SDK adapter is
// the sole implementation allowed to import SuroMesh SDK packages.
package ports

import (
	"context"
	"io"
	"time"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// UpstreamFactory constructs per-account upstream client sessions from
// decrypted session material. The ONLY seam to the SuroMesh SDK.
type UpstreamFactory interface {
	// CreateClient builds a live client session from decrypted session
	// state (or a fresh session when blob is empty). onPersist is invoked
	// by the adapter whenever the SDK signals that session state changed
	// (key exchange, authorization change); the runner persists the
	// returned blob through the encrypted session repository.
	CreateClient(ctx context.Context, spec ClientSpec) (ClientSession, error)
}

// ClientSpec describes one upstream client to construct.
type ClientSpec struct {
	AccountID domain.AccountID
	// SessionBlob is the decrypted service-owned session state (JSON v1);
	// empty for a brand-new session.
	SessionBlob []byte
	// OnPersist receives updated serialized session state whenever the
	// SDK signals a persistence-worthy change. Must be non-nil.
	OnPersist func(blob []byte) error
}

// ClientSession is one live upstream client owned by an account runner.
// All methods take ctx and return domain errors. Implementations must be
// safe for concurrent use (the SDK client is), with serialization handled
// by the runner for mutations.
type ClientSession interface {
	// Lifecycle
	Connect(ctx context.Context) error // blocking until connected/failed
	Disconnect(ctx context.Context) error
	Close(ctx context.Context) error
	Connected() bool

	// State
	AuthState() domain.AuthState

	// Authentication
	SendCode(ctx context.Context, phone string) error
	SubmitCode(ctx context.Context, phone, code string) error
	SubmitPassword(ctx context.Context, password string) error
	LogOut(ctx context.Context) error

	// Account / peers
	GetMe(ctx context.Context) (domain.Peer, error)
	ResolvePeer(ctx context.Context, ref domain.PeerRef) (domain.Peer, error)
	PeerDetails(ctx context.Context, ref domain.PeerRef, view domain.PeerView) (PeerDetailsResult, error)

	// Dialogs
	ListDialogs(ctx context.Context, pageSize int, token string) (domain.DialogPage, error)

	// Messaging
	SendMessage(ctx context.Context, ref domain.PeerRef, text string, replyTo *int64) (domain.Message, error)
	GetMessages(ctx context.Context, ref domain.PeerRef, ids []int64) (found []domain.Message, missing []int64, err error)
	EditMessage(ctx context.Context, ref domain.PeerRef, id int64, text string) (domain.Message, error)
	DeleteMessages(ctx context.Context, ref domain.PeerRef, ids []int64) ([]int64, error)
	ForwardMessages(ctx context.Context, from, to domain.PeerRef, ids []int64) ([]domain.Message, error)
	PinMessage(ctx context.Context, ref domain.PeerRef, id int64) (domain.Message, error)
	UnpinMessage(ctx context.Context, ref domain.PeerRef, id int64) (domain.Message, error)
	MarkAsRead(ctx context.Context, ref domain.PeerRef, maxID int64) error
	SearchMessages(ctx context.Context, ref domain.PeerRef, query string, limit int) ([]domain.Message, error)
	StreamHistory(ctx context.Context, ref domain.PeerRef, limit int) (HistoryStream, error)

	// Updates
	SubscribeUpdates(ctx context.Context, h UpdateHandler) (unsubscribe func(), err error)
	CatchUp(ctx context.Context) error

	// Media
	GetMediaMetadata(ctx context.Context, ref domain.PeerRef, id int64) (domain.MediaSummary, error)
	UploadMedia(ctx context.Context, up UploadSpec, r io.Reader) (domain.Message, error)
	DownloadMedia(ctx context.Context, ref domain.PeerRef, id int64, w io.Writer) error
}

// HistoryStream iterates history newest-first.
type HistoryStream interface {
	Next(ctx context.Context) (domain.Message, bool)
	Err() error
}

// PeerDetailsResult carries whichever projection the view requested.
type PeerDetailsResult struct {
	User    *domain.UserDetails
	Channel *domain.ChannelDetails
	Chat    *domain.ChatDetails
}

// UpdateHandler receives one accepted upstream update. Implementations
// must not block.
type UpdateHandler func(ctx context.Context, ev domain.UpdateEvent)

// UploadSpec describes one media upload.
type UploadSpec struct {
	Peer       domain.PeerRef
	SizeBytes  int64
	FileName   string
	MimeType   string
	AsDocument bool
}

// SessionRepository persists encrypted session blobs. Blobs arrive
// already encrypted (envelope format); implementations never see
// plaintext key material.
type SessionRepository interface {
	Load(ctx context.Context, account domain.AccountID) (blob []byte, err error) // ErrNotFound when absent
	Store(ctx context.Context, account domain.AccountID, blob []byte) error
	Delete(ctx context.Context, account domain.AccountID) error // crypto-shred
}

// AccountRepository persists account bindings and desired state.
type AccountRepository interface {
	Create(ctx context.Context, a AccountRecord) (AccountRecord, error)
	// GetByID resolves within tenant scope; ErrNotFound when absent.
	GetByID(ctx context.Context, tenantID, account string) (AccountRecord, error)
	// GetByPhoneHash resolves within tenant scope (idempotent rebinding).
	GetByPhoneHash(ctx context.Context, tenantID, phoneHash string) (AccountRecord, error)
	List(ctx context.Context, tenantID string, limit int, offset int) ([]AccountRecord, error)
	SetStatus(ctx context.Context, tenantID, account string, s domain.AccountStatus) error
	SetDesiredState(ctx context.Context, tenantID, account string, s domain.DesiredState) error
	Delete(ctx context.Context, tenantID, account string) error
}

// AccountRecord is the durable account binding row.
type AccountRecord struct {
	AccountID    string // UUID
	TenantID     string // UUID
	DisplayAlias string
	PhoneHash    string // HMAC-SHA256 of the normalized phone (lookup)
	PhoneMask    string // "+98 ••• •• 21 77"-style display form
	Status       domain.AccountStatus
	Desired      domain.DesiredState
	CreatedAt    time.Time
}

// TenantRepository creates/resolves tenants (bootstrap + lookup).
type TenantRepository interface {
	Create(ctx context.Context, name string) (id string, err error)
	Get(ctx context.Context, id string) (TenantRecord, error)
}

// TenantRecord is the durable tenant row.
type TenantRecord struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// CredentialStore authenticates callers: API-key hash lookup.
// Implementations MUST use constant-time comparison on hash equality and
// MUST NOT store raw keys.
type CredentialStore interface {
	// LookupByPrefix finds candidate credential rows by the key's 8-char
	// prefix (index), then the caller verifies the full hash.
	LookupByHashPrefix(ctx context.Context, prefix string) ([]CredentialRecord, error)
}

// CredentialRecord is one API credential row (hash only; never raw).
type CredentialRecord struct {
	ID          string
	TenantID    string
	KeyHash     string // hex SHA-256 of the raw key
	Prefix      string // first 8 chars of the raw key (identification)
	Permissions []string
	Status      string // active | revoked
}

// OperationRepository implements durable idempotency (S6): records are
// scoped by tenant and keyed by the caller's idempotency key; Claim is
// atomic (single winner).
type OperationRepository interface {
	// Claim atomically inserts the operation; when a record with the same
	// (tenant, key) exists, it returns the existing record with
	// claimed=false (caller replays the recorded outcome).
	Claim(ctx context.Context, r OperationRecord) (existing OperationRecord, claimed bool, err error)
	// Finish records the terminal outcome for the claimed operation.
	Finish(ctx context.Context, tenantID, opID, outcome, resultRef string) error
}

// OperationRecord is one durable operation (idempotency) row.
type OperationRecord struct {
	ID          string // operation UUID
	TenantID    string
	AccountID   string
	Kind        string // e.g. "messaging.send"
	Key         string // caller idempotency key
	Fingerprint string // hash of request shape (key-reuse conflict detection)
	State       string // created|claimed|sent|completed|failed|ambiguous
	ResultRef   string // e.g. message key of a completed send
	UpdatedAt   time.Time
}

// AuditRecorder records security-relevant events (no payloads).
type AuditRecorder interface {
	Record(ctx context.Context, e AuditEvent) error
}

// AuditEvent is one audit record.
type AuditEvent struct {
	Time     time.Time
	Actor    string // credential id or "bootstrap"
	TenantID string
	Target   string // opaque id
	Action   string // e.g. "account.bound", "auth.code_requested"
	Outcome  string // success|failure|denied
	Detail   string // sanitized single-line detail
}

// SessionVault seals/unseals session blobs (envelope encryption, S4).
// Plaintext key material exists only inside these calls.
type SessionVault interface {
	Seal(ctx context.Context, plaintext []byte) (blob []byte, err error)
	Unseal(ctx context.Context, blob []byte) (plaintext []byte, err error)
}
