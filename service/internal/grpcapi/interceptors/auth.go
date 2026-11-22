// Package interceptors implements the gRPC authn/authz/telemetry/recovery
// interceptor chain.
//
// Authn model (ADR-0005): `authorization: Bearer sk_...` API keys. The
// credential store resolves the key hash → tenant + permissions; the
// resolved principal rides the context for handlers and use cases.
// Deny-by-default: missing/bad credentials fail every RPC except the
// standard health service (separate listener concern).
package interceptors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// authKeyMetadata is the canonical metadata key.
const authKeyMetadata = "authorization"

// Principal is the resolved caller identity riding the context.
type Principal struct {
	CredentialID string
	TenantID     string
	Permissions  map[string]bool
}

// HasPermission reports whether the principal holds the permission.
func (p *Principal) HasPermission(perm string) bool {
	return p != nil && p.Permissions[perm]
}

type principalKey struct{}

// PrincipalFromContext extracts the resolved principal (nil when absent).
func PrincipalFromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}

// withPrincipal attaches the principal.
func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// CredentialAuthenticator verifies API keys against the credential store.
// Raw keys are hashed (SHA-256) before lookup; hash comparison is
// constant-time per candidate (hex strings of equal length).
type CredentialAuthenticator struct {
	store ports.CredentialStore
	// localOverrides supports bootstrap/dev credentials that exist before
	// the database is reachable: hash → principal.
	mu        sync.RWMutex
	overrides map[string]*Principal // hash → principal
	now       func() time.Time
}

// NewCredentialAuthenticator builds the authenticator.
func NewCredentialAuthenticator(store ports.CredentialStore) *CredentialAuthenticator {
	return &CredentialAuthenticator{store: store, now: time.Now}
}

// RegisterOverride registers a config/bootstrap credential (dev path).
// The raw key is hashed immediately; callers must not retain it.
func (a *CredentialAuthenticator) RegisterOverride(credentialID, tenantID, rawKey string, perms []string) {
	sum := sha256.Sum256([]byte(rawKey))
	hash := hex.EncodeToString(sum[:])
	permSet := make(map[string]bool, len(perms))
	for _, p := range perms {
		permSet[p] = true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.overrides == nil {
		a.overrides = map[string]*Principal{}
	}
	a.overrides[hash] = &Principal{
		CredentialID: credentialID,
		TenantID:     tenantID,
		Permissions:  permSet,
	}
}

// bearerToken extracts the raw key from the authorization metadata.
func bearerToken(md metadata.MD) (string, bool) {
	values := md.Get(authKeyMetadata)
	if len(values) == 0 {
		return "", false
	}
	raw := values[0]
	const prefix = "Bearer "
	if !strings.HasPrefix(raw, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(raw, prefix)
	if len(token) < 8 || len(token) > 128 || !strings.HasPrefix(token, "sk_") {
		return "", false
	}
	return token, true
}

func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves a raw key to a principal (constant-time per
// candidate; deny-by-default).
func (a *CredentialAuthenticator) Authenticate(ctx context.Context, rawKey string) (*Principal, error) {
	hash := hashKey(rawKey)
	prefix := rawKey[:8]

	// Overrides first (bootstrap path works without storage).
	a.mu.RLock()
	p, ok := a.overrides[hash]
	a.mu.RUnlock()
	if ok {
		return p, nil
	}

	// Storage path: constant-time compare over the (small) candidate set
	// sharing the prefix — never an early-exit string equality on the hash.
	candidates, err := a.store.LookupByHashPrefix(ctx, prefix)
	if err != nil {
		return nil, status.Error(codes.Internal, "credential lookup failed")
	}
	for _, c := range candidates {
		if constantTimeHexEqual(c.KeyHash, hash) {
			permSet := make(map[string]bool, len(c.Permissions))
			for _, p := range c.Permissions {
				permSet[p] = true
			}
			return &Principal{
				CredentialID: c.ID,
				TenantID:     c.TenantID,
				Permissions:  permSet,
			}, nil
		}
	}
	return nil, status.Error(codes.Unauthenticated, "invalid credentials")
}

// constantTimeHexEqual compares two equal-length hex strings in constant
// time (subtle.ConstantTimeCompare semantics).
func constantTimeHexEqual(a, b string) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	ab, aerr := hex.DecodeString(a)
	bb, berr := hex.DecodeString(b)
	if aerr != nil || berr != nil {
		return false
	}
	return hmacEqual(ab, bb)
}

func hmacEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// RequirePermission builds an interceptor that (1) authenticates and (2)
// enforces one permission for the wrapped RPCs. Denials are uniform:
// UNAUTHENTICATED for missing/bad keys, PERMISSION_DENIED for scopes —
// with NOT_FOUND semantics preserved for account-scoped checks done in
// use cases (never here; the interceptor has no account context).
func RequirePermission(auth *CredentialAuthenticator, perm string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		principal, err := authenticateMetadata(ctx, auth, info.FullMethod)
		if err != nil {
			return nil, err
		}
		if principal == nil {
			return handler(ctx, req) // health (exempt)
		}
		if !principal.HasPermission(perm) {
			return nil, deny(perm)
		}
		return handler(withPrincipal(ctx, principal), req)
	}
}

// RequirePermissionStream is the streaming variant.
func RequirePermissionStream(auth *CredentialAuthenticator, perm string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		principal, err := authenticateMetadata(ss.Context(), auth, info.FullMethod)
		if err != nil {
			return err
		}
		if principal == nil {
			return handler(srv, ss) // health (exempt)
		}
		if !principal.HasPermission(perm) {
			return deny(perm)
		}
		return handler(srv, &principalStream{ServerStream: ss, ctx: withPrincipal(ss.Context(), principal)})
	}
}

// healthPrefixes are served without credentials (standard gRPC health
// protocol; no data beyond SERVING status).
var healthPrefixes = []string{"/grpc.health.v1.Health/"}

func isHealthMethod(fullMethod string) bool {
	for _, p := range healthPrefixes {
		if strings.HasPrefix(fullMethod, p) {
			return true
		}
	}
	return false
}

func authenticateMetadata(ctx context.Context, auth *CredentialAuthenticator, fullMethod string) (*Principal, error) {
	if isHealthMethod(fullMethod) {
		return nil, nil // health: no principal required
	}
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}
	raw, ok := bearerToken(md)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing or malformed credentials")
	}
	return auth.Authenticate(ctx, raw)
}

func deny(perm string) error {
	return status.Error(codes.PermissionDenied, "missing required permission: "+perm)
}

// principalStream carries the principal context into streaming handlers.
type principalStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *principalStream) Context() context.Context { return s.ctx }

// Recovery intercepts handler panics: panic-based ordinary control flow
// is forbidden; an escaping panic becomes Internal with the cause logged
// upstream (never returned raw to clients).
func Recovery(logger interface {
	Error(msg string, args ...any)
}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("handler panic recovered", "method", info.FullMethod, "panic", toString(r))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(ctx, req)
	}
}

// RecoveryStream is the streaming variant.
func RecoveryStream(logger interface {
	Error(msg string, args ...any)
}) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("stream handler panic recovered", "method", info.FullMethod, "panic", toString(r))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return handler(srv, ss)
	}
}

func toString(v any) string {
	if e, ok := v.(error); ok {
		return e.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "non-error panic value"
}

// TimeoutInterceptor enforces per-RPC deadlines when the caller did not
// set one (defense against unbounded upstream waits).
func TimeoutInterceptor(d time.Duration) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if _, ok := ctx.Deadline(); !ok && d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return handler(ctx, req)
	}
}

var _ = domain.ErrNotFound // (domain taxonomy referenced by handler mapping)
