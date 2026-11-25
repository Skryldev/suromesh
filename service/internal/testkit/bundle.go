package testkit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// Bundle groups deterministic in-memory ports for tests: sessions,
// accounts, credentials, and audit (each implementing its port).
type Bundle struct {
	Sessions    *FakeSessions
	Accounts    *FakeAccounts
	Credentials *FakeCredentials
}

func NewBundle() *Bundle {
	return &Bundle{
		Sessions:    NewFakeSessions(),
		Accounts:    NewFakeAccounts(),
		Credentials: NewFakeCredentials(),
	}
}

// ---- accounts ----------------------------------------------------------------

type FakeAccounts struct {
	mu      sync.Mutex
	byID    map[string]ports.AccountRecord
	byPhone map[string]ports.AccountRecord // key: tenant + "|" + phone_hash
	order   []string
}

func NewFakeAccounts() *FakeAccounts {
	return &FakeAccounts{byID: map[string]ports.AccountRecord{}, byPhone: map[string]ports.AccountRecord{}}
}

var acctSeq int

func (f *FakeAccounts) Create(ctx context.Context, a ports.AccountRecord) (ports.AccountRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := a.TenantID + "|" + a.PhoneHash
	if _, dup := f.byPhone[key]; dup {
		return ports.AccountRecord{}, fmt.Errorf("%w: account", domain.ErrAlreadyExists)
	}
	acctSeq++
	a.AccountID = fmt.Sprintf("%08x-0000-0000-0000-000000000000", acctSeq)
	a.CreatedAt = timeNow()
	f.byID[a.AccountID] = a
	f.byPhone[key] = a
	f.order = append(f.order, a.AccountID)
	return a, nil
}

func (f *FakeAccounts) GetByID(ctx context.Context, tenantID, accountID string) (ports.AccountRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[accountID]
	if !ok || a.TenantID != tenantID {
		return ports.AccountRecord{}, fmt.Errorf("%w: account", domain.ErrNotFound)
	}
	return a, nil
}

func (f *FakeAccounts) GetByPhoneHash(ctx context.Context, tenantID, phoneHash string) (ports.AccountRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byPhone[tenantID+"|"+phoneHash]
	if !ok {
		return ports.AccountRecord{}, fmt.Errorf("%w: account", domain.ErrNotFound)
	}
	return a, nil
}

func (f *FakeAccounts) List(ctx context.Context, tenantID string, limit, offset int) ([]ports.AccountRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ports.AccountRecord
	// newest first: reverse order slice
	for i := len(f.order) - 1; i >= 0 && len(out) < limit; i-- {
		a, ok := f.byID[f.order[i]]
		if !ok || a.TenantID != tenantID {
			continue
		}
		if offset > 0 {
			offset--
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *FakeAccounts) SetStatus(ctx context.Context, tenantID, accountID string, s domain.AccountStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[accountID]
	if !ok || a.TenantID != tenantID {
		return fmt.Errorf("%w: account", domain.ErrNotFound)
	}
	a.Status = s
	f.byID[accountID] = a
	return nil
}

func (f *FakeAccounts) SetDesiredState(ctx context.Context, tenantID, accountID string, d domain.DesiredState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[accountID]
	if !ok || a.TenantID != tenantID {
		return fmt.Errorf("%w: account", domain.ErrNotFound)
	}
	a.Desired = d
	f.byID[accountID] = a
	return nil
}

func (f *FakeAccounts) Delete(ctx context.Context, tenantID, accountID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.byID[accountID]
	if !ok || a.TenantID != tenantID {
		return fmt.Errorf("%w: account", domain.ErrNotFound)
	}
	delete(f.byID, accountID)
	delete(f.byPhone, a.TenantID+"|"+a.PhoneHash)
	for i, id := range f.order {
		if id == accountID {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
	return nil
}

// ---- credentials ----------------------------------------------------------------

type FakeCredentials struct {
	mu       sync.Mutex
	byPrefix map[string][]ports.CredentialRecord
}

func NewFakeCredentials() *FakeCredentials {
	return &FakeCredentials{byPrefix: map[string][]ports.CredentialRecord{}}
}

func (f *FakeCredentials) LookupByHashPrefix(ctx context.Context, prefix string) ([]ports.CredentialRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.CredentialRecord, 0, len(f.byPrefix[prefix]))
	out = append(out, f.byPrefix[prefix]...)
	return out, nil
}

// Register seeds one credential (test helper).
func (f *FakeCredentials) Register(rec ports.CredentialRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byPrefix[rec.Prefix] = append(f.byPrefix[rec.Prefix], rec)
}

func timeNow() time.Time { return time.Now() }
