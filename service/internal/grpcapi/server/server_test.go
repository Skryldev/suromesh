package server_test

// gRPC integration tests (Phase 25 master spec §18.4): in-process server
// + real generated clients over the fake upstream. No live credentials;
// the fake upstream's unscripted operations fail deterministically.

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/interceptors"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/server"
	"github.com/Skryldev/suromesh/service/internal/testkit"
)

const testKey = "sk_test_key_0000000000000001"
const testKeyReadOnly = "sk_test_key_readonly0002"

type fixture struct {
	t       *testing.T
	conn    *grpc.ClientConn
	manager *application.Manager
	factory *testkit.FakeFactory
	bundle  *testkit.Bundle
	authN   *interceptors.CredentialAuthenticator
	boundID string
	stop    func()
}

func (f *fixture) accountClient() v1.AccountServiceClient {
	return v1.NewAccountServiceClient(f.conn)
}

func (f *fixture) messagingClient() v1.MessagingServiceClient {
	return v1.NewMessagingServiceClient(f.conn)
}

func (f *fixture) updateClient() v1.UpdateServiceClient {
	return v1.NewUpdateServiceClient(f.conn)
}

func (f *fixture) mediaClient() v1.MediaServiceClient {
	return v1.NewMediaServiceClient(f.conn)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	factory := testkit.NewFakeFactory()
	vault := &testkit.FakeVault{}
	bundle := testkit.NewBundle()
	audit := &testkit.FakeAudit{}
	manager := application.NewManager(factory, vault, bundle.Sessions, audit,
		application.ManagerConfig{StreamBuffer: 8, ReadConcurrent: 4, MutationQueue: 8})
	authN := interceptors.NewCredentialAuthenticator(bundle.Credentials)
	authN.RegisterOverride("cred-1", "tenant-1", testKey,
		[]string{"accounts:read", "accounts:write", "auth:write", "messaging:read",
			"messaging:write", "dialogs:read", "media:transfer", "streams:read",
			"sessions:write"})

	accounts := application.NewAccountService(bundle.Accounts, bundle.Sessions, audit,
		application.PhoneHashKey{Key: []byte("test-phone-hmac-key-1234")}, manager)
	auth := application.NewAuthService(manager, bundle.Accounts, audit,
		application.PhoneHashKey{Key: []byte("test-phone-hmac-key-1234")})

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, serveCancel := context.WithCancel(context.Background())
	srv, err := server.New(server.Config{
		Addr:         lis.Addr().String(),
		UnaryTimeout: 5 * time.Second,
	}, server.Deps{
		Manager:  manager,
		Accounts: accounts,
		Auth:     auth,
		AuthN:    authN,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Serve on the already-created listener: start in background.
	go func() { _ = srv.ServeOn(serveCtx, lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	fx := &fixture{t: t, conn: conn, manager: manager, factory: factory,
		bundle: bundle, authN: authN}
	fx.stop = func() {
		_ = conn.Close()
		serveCancel()
		mgrStop, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = manager.StopAll(mgrStop)
	}
	t.Cleanup(fx.stop)
	return fx
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func authed(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+testKey)
}

func TestAuthN_MissingKeyFails(t *testing.T) {
	fx := newFixture(t)
	client := v1.NewAccountServiceClient(fx.conn)
	_, err := client.ListAccounts(context.Background(), &v1.ListAccountsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", status.Code(err))
	}
}

func TestAuthN_BadKeyFails(t *testing.T) {
	fx := newFixture(t)
	client := v1.NewAccountServiceClient(fx.conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer sk_wrong_key_00000001")
	_, err := client.ListAccounts(ctx, &v1.ListAccountsRequest{})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", status.Code(err))
	}
}

func TestAuthZ_MissingPermissionDenied(t *testing.T) {
	fx := newFixture(t)
	// A read-only credential: messaging:write NOT granted.
	fx.authN.RegisterOverride("cred-ro", "tenant-1", testKeyReadOnly,
		[]string{"accounts:read", "messaging:read"})
	client := v1.NewMessagingServiceClient(fx.conn)
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer "+testKeyReadOnly)
	_, err := client.SendMessage(ctx, &v1.SendMessageRequest{
		AccountId: "00000000-0000-0000-0000-000000000001",
		Peer:      &v1.PeerRef{Ref: &v1.PeerRef_Self{Self: true}},
		Text:      "x",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %s, want PermissionDenied", status.Code(err))
	}
	// Messaging reads DO pass authz (then fail NotFound on the unknown
	// account — no existence oracle break: the account genuinely does
	// not exist for this tenant).
	_, err = client.GetMessages(ctx, &v1.GetMessagesRequest{
		AccountId: "00000000-0000-0000-0000-000000000001",
		Peer:      &v1.PeerRef{Ref: &v1.PeerRef_Self{Self: true}},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("read code = %s, want NotFound", status.Code(err))
	}
}

func TestAccounts_BindListGet(t *testing.T) {
	fx := newFixture(t)
	client := v1.NewAccountServiceClient(fx.conn)
	ctx := authed(context.Background())

	bind, err := client.BindAccount(ctx, &v1.BindAccountRequest{
		Phone: "+15550001111", DisplayAlias: "ops-test",
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bind.Account.Status != v1.AccountStatus_ACCOUNT_STATUS_PENDING_AUTH {
		t.Fatalf("status = %s", bind.Account.Status)
	}
	if bind.Account.MaskedPhone == "" || bind.Account.MaskedPhone == "+15550001111" {
		t.Fatalf("masked phone = %q (raw phone must never be returned)", bind.Account.MaskedPhone)
	}
	// Idempotent rebind.
	bind2, err := client.BindAccount(ctx, &v1.BindAccountRequest{
		Phone: "+1 (555) 000-1111", DisplayAlias: "ops-test-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bind2.Existing || bind2.Account.AccountId != bind.Account.AccountId {
		t.Fatalf("rebind not idempotent: %+v", bind2)
	}
	list, err := client.ListAccounts(ctx, &v1.ListAccountsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(list.Accounts))
	}
	get, err := client.GetAccount(ctx, &v1.GetAccountRequest{AccountId: bind.Account.AccountId})
	if err != nil {
		t.Fatal(err)
	}
	if get.Account.DisplayAlias != "ops-test" {
		t.Fatalf("alias = %q", get.Account.DisplayAlias)
	}
	// Unknown account → NOT_FOUND (no existence oracle).
	_, err = client.GetAccount(ctx, &v1.GetAccountRequest{
		AccountId: "99999999-9999-9999-9999-999999999999"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", status.Code(err))
	}
	// Malformed account id → InvalidArgument.
	_, err = client.GetAccount(ctx, &v1.GetAccountRequest{AccountId: "nope"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %s, want InvalidArgument", status.Code(err))
	}
}

func TestAuthFlow_SendCodeRequiresLiveRunner(t *testing.T) {
	fx := newFixture(t)
	client := v1.NewAuthenticationServiceClient(fx.conn)
	ctx := authed(context.Background())
	// No runner started (the account was never bound): auth RPCs must
	// fail NOT_FOUND, not fabricate state.
	_, err := client.SendCode(ctx, &v1.SendCodeRequest{
		AccountId: "00000000-0000-0000-0000-000000000001",
		Phone:     "+15550001111",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %s, want NotFound", status.Code(err))
	}
}

func TestHealth_ServedWithoutCredentials(t *testing.T) {
	fx := newFixture(t)
	client := healthpb.NewHealthClient(fx.conn)
	resp, err := client.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if resp.Status != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("status = %s", resp.Status)
	}
}

func TestManager_Isolation_TenantScopedGet(t *testing.T) {
	fx := newFixture(t)
	// Runner for tenant-1; manager.Get with another tenant → ErrNotFound.
	if _, err := fx.manager.StartRunner(context.Background(),
		application.RunnerConfig{AccountID: domain.AccountID("acc-x"), TenantID: "tenant-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.manager.Get("tenant-2", domain.AccountID("acc-x")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
	}
}

// FakeAudit + storage seams used by the fixture (deterministic).
var _ = atomic.Bool{}
