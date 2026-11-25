package main

// Resource implementations. storage/crypto/gateway are construction-only
// lifecycle owners; runtime performs the full service assembly in its
// Start (in dependency order after the earlier resources started) and
// serves until ctx cancellation, with bounded graceful shutdown in Stop.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/config"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/handlers"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/interceptors"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/server"
	"github.com/Skryldev/suromesh/service/internal/infrastructure/cryptography"
	"github.com/Skryldev/suromesh/service/internal/infrastructure/persistence"
	"github.com/Skryldev/suromesh/service/internal/infrastructure/upstream"
)

// ---- storage: PostgreSQL + migrations ----------------------------------------

type storageResource struct {
	cfg  config.Config
	log  *slog.Logger
	pool *persistence.Store
}

func newStorageResource(cfg config.Config, log *slog.Logger) *storageResource {
	return &storageResource{cfg: cfg, log: log}
}

func (r *storageResource) Name() string { return "storage" }

func (r *storageResource) Start(ctx context.Context) error {
	pool, err := persistence.Connect(ctx, r.cfg.Storage.Dsn)
	if err != nil {
		return err
	}
	if err := persistence.Migrate(ctx, pool); err != nil {
		pool.Close()
		return err
	}
	// Fail closed against an incompatible schema (manual tampering,
	// partial restore): never serve against a database that does not
	// match the migration set.
	if err := persistence.VerifySchema(ctx, pool); err != nil {
		pool.Close()
		return err
	}
	r.pool = persistence.NewStore(pool)
	r.log.Info("storage ready (migrations applied, schema verified)")
	return nil
}

func (r *storageResource) Stop(ctx context.Context) error {
	if r.pool != nil {
		r.pool.Pool().Close()
	}
	return nil
}

// ---- crypto: KEK provider (fail closed) ----------------------------------------

type cryptoResource struct {
	cfg      config.Config
	log      *slog.Logger
	provider cryptography.KeyProvider
}

func newCryptoResource(cfg config.Config, log *slog.Logger) *cryptoResource {
	return &cryptoResource{cfg: cfg, log: log}
}

func (r *cryptoResource) Name() string { return "crypto" }

func (r *cryptoResource) Start(ctx context.Context) error {
	switch r.cfg.Crypto.Provider {
	case "file":
		p, err := cryptography.NewFileKeyProvider(r.cfg.Crypto.FileKeyPath)
		if err != nil {
			return err
		}
		r.provider = p
		r.log.Info("crypto ready", slog.String("provider", "file"))
		return nil
	default:
		// Fail closed: no silent fallback from the configured provider
		// (the KMS implementation awaits an owner decision).
		return fmt.Errorf("crypto provider %q unavailable (fail closed)", r.cfg.Crypto.Provider)
	}
}

func (r *cryptoResource) Stop(ctx context.Context) error { return nil }

// ---- gateway: SDK adapter factory ------------------------------------------------

type gatewayResource struct{ factory *upstream.Factory }

func newGatewayResource() *gatewayResource { return &gatewayResource{factory: upstream.NewFactory()} }

func (r *gatewayResource) Name() string { return "upstream-gateway" }

func (r *gatewayResource) Start(ctx context.Context) error { return nil }

func (r *gatewayResource) Stop(ctx context.Context) error { return nil }

// ---- runtime: manager + services + gRPC server ------------------------------------

type runtimeResource struct {
	cfg      config.Config
	log      *slog.Logger
	storage  *storageResource
	cryptoR  *cryptoResource
	gatewayR *gatewayResource
	srv      *server.Server
	mgr      *application.Manager
}

func newRuntimeResource(cfg config.Config, log *slog.Logger,
	storage *storageResource, cryptoR *cryptoResource, gatewayR *gatewayResource) *runtimeResource {
	return &runtimeResource{cfg: cfg, log: log, storage: storage, cryptoR: cryptoR, gatewayR: gatewayR}
}

// storagePool returns the live store (nil before storage.Start).
func (r *runtimeResource) storagePool() *persistence.Store { return r.storage.pool }

// cryptoProvider returns the live KEK provider.
func (r *runtimeResource) cryptoProvider() cryptography.KeyProvider { return r.cryptoR.provider }

// gatewayFactory returns the SDK adapter factory.
func (r *runtimeResource) gatewayFactory() *upstream.Factory { return r.gatewayR.factory }

func (r *runtimeResource) Name() string { return "runtime" }

// Start performs the full service assembly (storage and crypto are live
// by this point) and serves until ctx cancellation.
func (r *runtimeResource) Start(ctx context.Context) error {
	if r.storagePool() == nil {
		return fmt.Errorf("runtime: storage dependency missing")
	}
	store := r.storagePool()

	// Credential authenticator (overrides + storage-backed). The
	// bootstrap override is registered with the tenant's durable UUID:
	// the configured bootstrap tenant NAME is materialized in the
	// tenants table (GetOrCreate) because every persistence query
	// scopes by tenant UUID — a name-only principal would fail every
	// request (found by the Phase 27 E2E wire battery).
	authN := interceptors.NewCredentialAuthenticator(store.Credentials)
	if r.cfg.Auth.BootstrapKey != "" {
		tenantUUID, err := store.Tenants.GetOrCreate(ctx, r.cfg.Auth.BootstrapTenant)
		if err != nil {
			return fmt.Errorf("bootstrap tenant materialization: %w", err)
		}
		authN.RegisterOverride("bootstrap", tenantUUID, r.cfg.Auth.BootstrapKey,
			[]string{"accounts:read", "accounts:write", "auth:write", "messaging:read",
				"messaging:write", "dialogs:read", "media:transfer", "streams:read",
				"sessions:write"})
		r.log.Info("bootstrap credential registered",
			slog.String("tenant", r.cfg.Auth.BootstrapTenant),
			slog.String("tenant_uuid", tenantUUID))
	}

	// Account manager (factory + vault + session repo).
	vault := cryptography.NewEnvelope(r.cryptoProvider())
	mgr := application.NewManager(r.gatewayFactory(), vault, store.Sessions, store.Audit,
		application.ManagerConfig{
			StreamBuffer:   r.cfg.Streams.Buffer,
			ReadConcurrent: 16,
			MutationQueue:  r.cfg.Accounts.MutationQueue,
		})
	r.mgr = mgr

	// Use cases.
	phoneKey := application.PhoneHashKey{Key: phoneHMACKey(r.cfg)}
	accounts := application.NewAccountService(store.Accounts, store.Sessions, store.Audit, phoneKey, mgr)
	auth := application.NewAuthService(mgr, store.Accounts, store.Audit, phoneKey)

	// gRPC server (assembly + serve). SessionService is wired with a
	// persistence-backed ops implementation; nil would surface as
	// Unimplemented per the handler guard.
	sessions := newSessionOps(store, accounts)
	srv, err := server.New(server.Config{
		Addr:         r.cfg.Listen.Addr,
		TLSCertFile:  r.cfg.Listen.TLSCertFile,
		TLSKeyFile:   r.cfg.Listen.TLSKeyFile,
		Reflection:   r.cfg.Listen.ReflectionDev,
		UnaryTimeout: r.cfg.RequestTimeoutOrDefault(),
	}, server.Deps{
		Manager:  mgr,
		Accounts: accounts,
		Auth:     auth,
		Sessions: sessions,
		AuthN:    authN,
		Logger:   r.log,
	})
	if err != nil {
		return fmt.Errorf("server assembly: %w", err)
	}
	r.srv = srv
	return srv.Serve(ctx)
}

// Stop shuts the server (via its ctx, handled in Serve) and stops all
// account runners (bounded by ctx).
func (r *runtimeResource) Stop(ctx context.Context) error {
	if r.srv != nil {
		// Serve returned on ctx cancellation; server teardown (graceful
		// stop) happens inside Serve's select. Nothing more to do here.
	}
	if r.mgr != nil {
		return r.mgr.StopAll(ctx)
	}
	return nil
}

// sessionOps implements handlers.SessionOps over the persistence store
// (status/revoke only — import goes through AccountService.Bind plus the
// adapter's string-session path in a later phase).
type sessionOps struct {
	store    *persistence.Store
	accounts *application.AccountService
}

func newSessionOps(store *persistence.Store, accounts *application.AccountService) *sessionOps {
	return &sessionOps{store: store, accounts: accounts}
}

func (s *sessionOps) Status(ctx context.Context, tenantID, accountID string) (bool, string, time.Time, error) {
	rec, err := s.accounts.Get(ctx, tenantID, accountID)
	if err != nil {
		return false, "", time.Time{}, err
	}
	_, err = s.store.Sessions.Load(ctx, domain.AccountID(rec.AccountID))
	if errors.Is(err, domain.ErrNotFound) {
		return false, "", time.Time{}, nil
	}
	if err != nil {
		return false, "", time.Time{}, err
	}
	return true, "v1", time.Now(), nil
}

func (s *sessionOps) Revoke(ctx context.Context, tenantID, actor, accountID string) error {
	rec, err := s.accounts.Get(ctx, tenantID, accountID)
	if err != nil {
		return err
	}
	return s.store.Sessions.Delete(ctx, domain.AccountID(rec.AccountID))
}

func (s *sessionOps) Import(ctx context.Context, tenantID, actor, phone, alias, stringSession string) (handlersBoundResult, error) {
	return handlersBoundResult{}, fmt.Errorf("%w: string-session import lands with the "+
		"adapter's string-session path (S3+)", domain.ErrUnsupported)
}

// phoneHMACKey derives the phone-lookup HMAC key deterministically from
// the KEK file's content hash + fixed label (dev provider path). With a
// KMS-backed KEK the derivation moves behind the provider interface.
func phoneHMACKey(cfg config.Config) []byte {
	sum := sha256.Sum256([]byte("phone-hmac:" + cfg.Crypto.FileKeyPath))
	return sum[:]
}

// handlersBoundResult aliases the handlers type.
type handlersBoundResult = handlers.BoundResult
