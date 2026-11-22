package persistence_test

// Integration tests against real PostgreSQL (Phase 25 master spec §18.3).
// These run ONLY when TEST_DATABASE_DSN is set (CI provides a postgres
// service container); locally they skip with an explicit message.

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/infrastructure/persistence"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("INTEGRATION SKIPPED — TEST_DATABASE_DSN not set (CI provides a postgres container)")
	}
	return dsn
}

func newStore(t *testing.T) *persistence.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := persistence.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := persistence.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Clean slate per test: deterministic isolation.
	if _, err := pool.Exec(ctx, `TRUNCATE audit_events, operations, sessions, accounts, credentials, tenants CASCADE`); err != nil {
		t.Fatal(err)
	}
	return persistence.NewStore(pool)
}

func TestPersistence_TenantLifecycle(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	id, err := store.Tenants.Create(ctx, "tenant-a")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := store.Tenants.Get(ctx, id)
	if err != nil || rec.Name != "tenant-a" {
		t.Fatalf("tenant round trip: %v %+v", err, rec)
	}
	// Duplicate name rejected.
	if _, err := store.Tenants.Create(ctx, "tenant-a"); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("dup tenant err = %v, want ErrAlreadyExists", err)
	}
}

func TestPersistence_AccountScopedAccess(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	t1, _ := store.Tenants.Create(ctx, "t1")
	t2, _ := store.Tenants.Create(ctx, "t2")

	rec, err := store.Accounts.Create(ctx, ports.AccountRecord{
		TenantID: t1, DisplayAlias: "a", PhoneHash: "h1", PhoneMask: "+98 *** 11",
		Status: domain.AccountStatusPendingAuth, Desired: domain.DesiredStopped,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Owner reads work.
	if _, err := store.Accounts.GetByID(ctx, t1, rec.AccountID); err != nil {
		t.Fatalf("owner read: %v", err)
	}
	// Cross-tenant read is NOT_FOUND.
	if _, err := store.Accounts.GetByID(ctx, t2, rec.AccountID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant read = %v, want ErrNotFound", err)
	}
	// Same phone in another tenant is allowed (tenant-scoped unique).
	if _, err := store.Accounts.Create(ctx, ports.AccountRecord{
		TenantID: t2, DisplayAlias: "b", PhoneHash: "h1", PhoneMask: "+98 *** 11",
		Status: domain.AccountStatusPendingAuth, Desired: domain.DesiredStopped,
	}); err != nil {
		t.Fatalf("same phone across tenants must be allowed: %v", err)
	}
	// Same phone in the SAME tenant is rejected (unique constraint).
	if _, err := store.Accounts.Create(ctx, ports.AccountRecord{
		TenantID: t1, DisplayAlias: "c", PhoneHash: "h1", PhoneMask: "+98 *** 11",
		Status: domain.AccountStatusPendingAuth, Desired: domain.DesiredStopped,
	}); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("same-phone dup err = %v, want ErrAlreadyExists", err)
	}
	// Delete cascades the session row (FK ON DELETE CASCADE).
	if err := store.Sessions.Store(ctx, domain.AccountID(rec.AccountID), []byte("enc")); err != nil {
		t.Fatal(err)
	}
	if err := store.Accounts.Delete(ctx, t1, rec.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Sessions.Load(ctx, domain.AccountID(rec.AccountID)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("session must cascade-delete with the account")
	}
}

func TestPersistence_SessionStoreLoad(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	tid, _ := store.Tenants.Create(ctx, "t")
	rec, _ := store.Accounts.Create(ctx, ports.AccountRecord{
		TenantID: tid, DisplayAlias: "a", PhoneHash: "h", PhoneMask: "m",
		Status: domain.AccountStatusActive, Desired: domain.DesiredStopped,
	})
	acc := domain.AccountID(rec.AccountID)
	blob := []byte{0x53, 0x4d, 0x42, 0x31, 0x01, 0xde, 0xad}
	if err := store.Sessions.Store(ctx, acc, blob); err != nil {
		t.Fatal(err)
	}
	got, err := store.Sessions.Load(ctx, acc)
	if err != nil || string(got) != string(blob) {
		t.Fatalf("session round trip: %v %x", err, got)
	}
	if err := store.Sessions.Delete(ctx, acc); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Sessions.Load(ctx, acc); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("shredded session must be gone")
	}
}

func TestPersistence_IdempotencyClaimAndRace(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	tid, _ := store.Tenants.Create(ctx, "t")
	rec, _ := store.Accounts.Create(ctx, ports.AccountRecord{
		TenantID: tid, DisplayAlias: "a", PhoneHash: "h", PhoneMask: "m",
		Status: domain.AccountStatusActive, Desired: domain.DesiredStopped,
	})
	op := ports.OperationRecord{
		TenantID: tid, AccountID: rec.AccountID, Kind: "messaging.send",
		Key: "key-1", Fingerprint: "fp-1",
	}
	claimed, ok, err := store.Operations.Claim(ctx, op)
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	// Replay returns the existing record.
	existing, ok, err := store.Operations.Claim(ctx, op)
	if err != nil || ok {
		t.Fatalf("replay: ok=%v err=%v", ok, err)
	}
	if existing.ID != claimed.ID {
		t.Fatal("replay must return the original record")
	}
	// Key reuse with different fingerprint → conflict.
	op2 := op
	op2.Fingerprint = "fp-different"
	if _, _, err := store.Operations.Claim(ctx, op2); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("fingerprint conflict err = %v, want ErrAlreadyExists", err)
	}
	// Finish → replay carries the outcome.
	if err := store.Operations.Finish(ctx, tid, claimed.ID, "completed", "user:1#42"); err != nil {
		t.Fatal(err)
	}
	existing, _, err = store.Operations.Claim(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	if existing.State != "completed" || existing.ResultRef != "user:1#42" {
		t.Fatalf("replay state = %s ref = %s", existing.State, existing.ResultRef)
	}
}

func TestPersistence_IdempotencyConcurrentClaims(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	tid, _ := store.Tenants.Create(ctx, "t")
	rec, _ := store.Accounts.Create(ctx, ports.AccountRecord{
		TenantID: tid, DisplayAlias: "a", PhoneHash: "h", PhoneMask: "m",
		Status: domain.AccountStatusActive, Desired: domain.DesiredStopped,
	})
	op := ports.OperationRecord{
		TenantID: tid, AccountID: rec.AccountID, Kind: "messaging.send",
		Key: "race-1", Fingerprint: "fp",
	}
	const n = 8
	winners := make([]bool, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, claimed, err := store.Operations.Claim(ctx, op)
			mu.Lock()
			winners[i] = err == nil && claimed
			mu.Unlock()
			_ = err
		}(i)
	}
	close(start)
	wg.Wait()
	won := 0
	for _, w := range winners {
		if w {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("concurrent claims: %d winners, want exactly 1", won)
	}
}

func TestPersistence_AuditRecorded(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	err := store.Audit.Record(ctx, ports.AuditEvent{
		Time: time.Now(), Actor: "cred-1", TenantID: "", Target: "x",
		Action: "account.bound", Outcome: "success",
	})
	if err != nil {
		t.Fatal(err)
	}
	// No assertion on readback (audit is append-only; the service's
	// observability reads it via SQL directly).
}
