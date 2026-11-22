package testkit

import (
	"context"
	"sync"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// FakeFactory is the scriptable ports.UpstreamFactory: it hands out
// FakeClient sessions and records every created spec. Create behavior is
// scriptable per factory (e.g. simulate upstream unavailability).
type FakeFactory struct {
	mu       sync.Mutex
	clients  map[string]*FakeClient
	OnCreate func(spec ports.ClientSpec) (ports.ClientSession, error)
	Created  []ports.ClientSpec
}

func NewFakeFactory() *FakeFactory {
	return &FakeFactory{clients: map[string]*FakeClient{}}
}

func (f *FakeFactory) CreateClient(ctx context.Context, spec ports.ClientSpec) (ports.ClientSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Created = append(f.Created, spec)
	if f.OnCreate != nil {
		return f.OnCreate(spec)
	}
	c := &FakeClient{}
	f.clients[string(spec.AccountID)] = c
	return c, nil
}

// ClientFor returns the fake client created for an account (nil if none).
func (f *FakeFactory) ClientFor(account domain.AccountID) *FakeClient {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.clients[string(account)]
}

// ClientCount returns the number of created clients.
func (f *FakeFactory) ClientCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Created)
}

// FakeVault is an in-memory SessionVault (deterministic; no real crypto).
type FakeVault struct{ mu sync.Mutex }

func (f *FakeVault) Seal(ctx context.Context, plaintext []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(plaintext))
	for i, b := range plaintext {
		out[i] = b ^ 0xA5
	}
	return out, nil
}

func (f *FakeVault) Unseal(ctx context.Context, blob []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(blob))
	for i, b := range blob {
		out[i] = b ^ 0xA5
	}
	return out, nil
}

// FakeSessions is an in-memory SessionRepository.
type FakeSessions struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func NewFakeSessions() *FakeSessions { return &FakeSessions{blobs: map[string][]byte{}} }

func (f *FakeSessions) Load(ctx context.Context, account domain.AccountID) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blobs[string(account)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return b, nil
}

func (f *FakeSessions) Store(ctx context.Context, account domain.AccountID, blob []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blobs[string(account)] = blob
	return nil
}

func (f *FakeSessions) Delete(ctx context.Context, account domain.AccountID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.blobs, string(account))
	return nil
}
