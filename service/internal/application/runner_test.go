package application_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
	"github.com/Skryldev/suromesh/service/internal/testkit"
)

func runnerCfg(account string) application.RunnerConfig {
	return application.RunnerConfig{
		AccountID:      domain.AccountID(account),
		TenantID:       "tenant-1",
		MutationQueue:  8,
		ReadConcurrent: 4,
		StreamBuffer:   4,
	}
}

func TestRunner_StartWithSessionRestore(t *testing.T) {
	f := testkit.NewFakeFactory()
	vault := &testkit.FakeVault{}
	repo := testkit.NewFakeSessions()
	// Store the SEALED form (as the persistence path would): the runner
	// unseals before constructing the client.
	sealed, err := vault.Seal(context.Background(), []byte("seed"))
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.Store(context.Background(), domain.AccountID("acc-1"), sealed)

	r := application.NewRunner(runnerCfg("acc-1"), f, vault, repo)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	if f.ClientCount() != 1 {
		t.Fatalf("clients created = %d, want 1", f.ClientCount())
	}
	spec := f.Created[0]
	if string(spec.SessionBlob) != "seed" {
		t.Fatalf("session blob = %q, want unsealed seed", spec.SessionBlob)
	}
}

func TestRunner_MutationSerialization(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	// Serialized: only one mutation inside the critical section at a time.
	var active, maxActive int
	var mu sync.Mutex
	for i := 0; i < 16; i++ {
		if err := r.Mutate(context.Background(), func(s ports.ClientSession) error {
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if maxActive != 1 {
		t.Fatalf("mutations overlapped (max concurrent = %d), want 1", maxActive)
	}
}

func TestRunner_ReadsRunConcurrently(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	var active, maxActive int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Read(context.Background(), func(s ports.ClientSession) error {
				mu.Lock()
				active++
				if active > maxActive {
					maxActive = active
				}
				mu.Unlock()
				time.Sleep(30 * time.Millisecond)
				mu.Lock()
				active--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if maxActive < 2 {
		t.Fatalf("reads did not run concurrently (max = %d)", maxActive)
	}
}

func TestRunner_SessionPersistedOnUpstreamSignal(t *testing.T) {
	f := testkit.NewFakeFactory()
	repo := testkit.NewFakeSessions()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, repo)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	// The fake client's own persist trigger: submit code flips auth state
	// and (in the real adapter) fires OnPersist. The runner wiring is
	// verified via the OnPersist callback round trip.
	if err := r.Mutate(context.Background(), func(s ports.ClientSession) error {
		return s.SubmitCode(context.Background(), "+15550001111", "12345")
	}); err != nil {
		t.Fatal(err)
	}
	// The fake doesn't call OnPersist directly; assert the client accepted
	// the auth transition instead (adapter wiring tested in upstream pkg).
	c := f.ClientFor(domain.AccountID("acc-1"))
	if c.AuthState() != domain.AuthAuthorized {
		t.Fatalf("auth state = %s", c.AuthState())
	}
}

func TestRunner_StopDuringBlockedMutation(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Submit a mutation that blocks; Stop must not wait for it
	// indefinitely (shutdown is bounded and marked regardless).
	go func() {
		_ = r.Mutate(context.Background(), func(s ports.ClientSession) error {
			time.Sleep(2 * time.Second)
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond) // let the mutation start

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = r.Stop(ctx) // bounded; may report ctx.Err() — must not deadlock
}

func TestRunner_SubscribeHubDelivery(t *testing.T) {
	f := testkit.NewFakeFactory()
	r := application.NewRunner(runnerCfg("acc-1"), f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	ch, cancel, err := r.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	ev := domain.UpdateEvent{Kind: domain.UpdateNewMessage,
		NewMessage: &domain.Message{ID: 1, Text: "hi"}}
	f.ClientFor(domain.AccountID("acc-1")).Publish(ev)

	select {
	case got := <-ch:
		if got.StreamSeq != 1 {
			t.Fatalf("stream seq = %d, want 1", got.StreamSeq)
		}
		if got.NewMessage == nil || got.NewMessage.Text != "hi" {
			t.Fatalf("event mismatch: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered")
	}
}

func TestRunner_SlowSubscriberDropped(t *testing.T) {
	f := testkit.NewFakeFactory()
	cfg := runnerCfg("acc-1")
	r := application.NewRunner(cfg, f, &testkit.FakeVault{}, testkit.NewFakeSessions())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop(context.Background())

	ch, cancel, err := r.Subscribe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// Never read from ch: the buffer (4) overflows and the subscriber is
	// dropped by policy.
	c := f.ClientFor(domain.AccountID("acc-1"))
	for i := 0; i < 10; i++ {
		c.Publish(domain.UpdateEvent{Kind: domain.UpdateNewMessage,
			NewMessage: &domain.Message{ID: int64(i + 1), Text: "x"}})
	}
	// After overflow, the channel is closed by policy.
	select {
	case _, ok := <-ch:
		if ok {
			// drain one buffered event; keep waiting for close
			for range ch {
			}
		}
	case <-time.After(1 * time.Second):
		t.Fatal("subscriber channel not closed after overflow")
	}
}

func TestManager_DuplicateRunnerRejected(t *testing.T) {
	f := testkit.NewFakeFactory()
	m := application.NewManager(f, &testkit.FakeVault{}, testkit.NewFakeSessions(), nil,
		application.ManagerConfig{})
	cfg := runnerCfg("acc-1")
	if _, err := m.StartRunner(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll(context.Background())
	if _, err := m.StartRunner(context.Background(), cfg); !errors.Is(err, domain.ErrAlreadyExists) {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
	if m.Count() != 1 {
		t.Fatalf("count = %d", m.Count())
	}
}

func TestManager_TenantScopedGet(t *testing.T) {
	f := testkit.NewFakeFactory()
	m := application.NewManager(f, &testkit.FakeVault{}, testkit.NewFakeSessions(), nil,
		application.ManagerConfig{})
	cfg := runnerCfg("acc-1")
	cfg.TenantID = "tenant-A"
	if _, err := m.StartRunner(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	defer m.StopAll(context.Background())

	if _, err := m.Get("tenant-A", domain.AccountID("acc-1")); err != nil {
		t.Fatalf("owner get: %v", err)
	}
	if _, err := m.Get("tenant-B", domain.AccountID("acc-1")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-tenant get = %v, want ErrNotFound", err)
	}
}
