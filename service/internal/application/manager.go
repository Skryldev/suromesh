package application

import (
	"context"
	"fmt"
	"sync"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// updateHub fans accepted updates out to bounded subscriber queues.
// Overflow policy: the offending subscriber is dropped (its channel
// closed); other subscribers and the account pipeline are unaffected.
type updateHub struct {
	mu     sync.Mutex
	buffer int
	subs   map[int64]*hubSub
	next   int64
	closed bool
}

type hubSub struct {
	ch      chan domain.UpdateEvent
	seq     int64
	dropped bool
}

func newUpdateHub(buffer int) *updateHub {
	if buffer <= 0 {
		buffer = 1024
	}
	return &updateHub{buffer: buffer, subs: map[int64]*hubSub{}}
}

func (h *updateHub) subscribe() (<-chan domain.UpdateEvent, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, nil, fmt.Errorf("%w: hub closed", domain.ErrAccountStopped)
	}
	h.next++
	id := h.next
	sub := &hubSub{ch: make(chan domain.UpdateEvent, h.buffer)}
	h.subs[id] = sub
	return sub.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s, ok := h.subs[id]; ok {
			close(s.ch)
			delete(h.subs, id)
		}
	}, nil
}

func (h *updateHub) publish(ev domain.UpdateEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for id, s := range h.subs {
		if s.dropped {
			continue
		}
		s.seq++
		ev.StreamSeq = s.seq
		select {
		case s.ch <- ev:
		default:
			// Slow consumer: drop the subscriber (documented policy).
			s.dropped = true
			close(s.ch)
			delete(h.subs, id)
		}
	}
}

func (h *updateHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, s := range h.subs {
		if !s.dropped {
			close(s.ch)
		}
		delete(h.subs, id)
	}
}

// Manager owns all account runners in this process. It enforces:
//   - one runner per account (duplicates rejected);
//   - tenant-scoped access (Get resolves through the caller's tenant);
//   - per-runner mutation pumps (one goroutine per runner).
//
// Single-replica ownership (ADR-0002): this manager is the in-process
// owner; multi-replica leasing is a designed future extension —
// in-memory ownership is never claimed as distributed safety.
type Manager struct {
	mu        sync.RWMutex
	runners   map[string]*Runner // key: account UUID
	starting  map[string]*startFlight
	factor    ports.UpstreamFactory
	vault     ports.SessionVault
	repo      ports.SessionRepository
	audit     ports.AuditRecorder
	streamBuf int
	readCap   int
	mutQueue  int
}

// startFlight coordinates concurrent StartRunner calls for the SAME
// account: exactly one upstream client is constructed; losers observe the
// winner's outcome instead of racing to double-create.
type startFlight struct {
	done   chan struct{}
	runner *Runner // set when the start succeeded
	err    error   // set when the start failed
}

// ManagerConfig bounds manager-wide defaults.
type ManagerConfig struct {
	StreamBuffer   int
	ReadConcurrent int
	MutationQueue  int
}

// NewManager builds the manager (no runners yet).
func NewManager(factor ports.UpstreamFactory, vault ports.SessionVault,
	repo ports.SessionRepository, audit ports.AuditRecorder, cfg ManagerConfig) *Manager {
	if cfg.StreamBuffer <= 0 {
		cfg.StreamBuffer = 1024
	}
	if cfg.ReadConcurrent <= 0 {
		cfg.ReadConcurrent = 16
	}
	if cfg.MutationQueue <= 0 {
		cfg.MutationQueue = 64
	}
	return &Manager{
		runners:   map[string]*Runner{},
		starting:  map[string]*startFlight{},
		factor:    factor,
		vault:     vault,
		repo:      repo,
		audit:     audit,
		streamBuf: cfg.StreamBuffer,
		readCap:   cfg.ReadConcurrent,
		mutQueue:  cfg.MutationQueue,
	}
}

// StartRunner creates and starts the runtime for one account. Duplicate
// creation returns ErrAlreadyExists deterministically. Concurrent calls
// for the same account share one start attempt: exactly one client is
// constructed (the in-flight guard below), losers get ErrAlreadyExists
// on success or the underlying start error on failure.
func (m *Manager) StartRunner(ctx context.Context, cfg RunnerConfig) (*Runner, error) {
	key := string(cfg.AccountID)
	m.mu.Lock()
	if _, dup := m.runners[key]; dup {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: runner for %s already exists",
			domain.ErrAlreadyExists, cfg.AccountID)
	}
	if flight, inFlight := m.starting[key]; inFlight {
		m.mu.Unlock()
		select {
		case <-flight.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if flight.runner != nil {
			return nil, fmt.Errorf("%w: runner for %s already exists",
				domain.ErrAlreadyExists, cfg.AccountID)
		}
		return nil, flight.err
	}
	flight := &startFlight{done: make(chan struct{})}
	m.starting[key] = flight
	m.mu.Unlock()

	finish := func(r *Runner, err error) (*Runner, error) {
		m.mu.Lock()
		flight.runner, flight.err = r, err
		delete(m.starting, key)
		close(flight.done)
		m.mu.Unlock()
		return r, err
	}

	r := NewRunner(cfg, m.factor, m.vault, m.repo)
	if err := r.Start(ctx); err != nil {
		return finish(nil, err)
	}

	m.mu.Lock()
	if _, dup := m.runners[key]; dup { // re-check under lock
		m.mu.Unlock()
		_ = r.Stop(ctx)
		return finish(nil, fmt.Errorf("%w: runner for %s already exists",
			domain.ErrAlreadyExists, cfg.AccountID))
	}
	m.runners[key] = r
	m.mu.Unlock()
	return finish(r, nil)
}

// Get resolves a runner within the tenant scope. A runner from another
// tenant (or unknown) is NOT_FOUND — no existence oracle.
func (m *Manager) Get(tenantID string, account domain.AccountID) (*Runner, error) {
	m.mu.RLock()
	r, ok := m.runners[string(account)]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", domain.ErrNotFound, account)
	}
	if r.cfg.TenantID != tenantID {
		return nil, fmt.Errorf("%w: %s", domain.ErrNotFound, account)
	}
	return r, nil
}

// StopRunner stops and removes one runner (idempotent).
func (m *Manager) StopRunner(ctx context.Context, tenantID string, account domain.AccountID) error {
	m.mu.Lock()
	r, ok := m.runners[string(account)]
	if ok && r.cfg.TenantID != tenantID {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", domain.ErrNotFound, account)
	}
	if ok {
		delete(m.runners, string(account))
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", domain.ErrNotFound, account)
	}
	return r.Stop(ctx)
}

// StopAll stops every runner (shutdown path). Errors joined.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	all := make([]*Runner, 0, len(m.runners))
	for _, r := range m.runners {
		all = append(all, r)
	}
	m.runners = map[string]*Runner{}
	m.mu.Unlock()
	var joined error
	for _, r := range all {
		if err := r.Stop(ctx); err != nil {
			joined = fmt.Errorf("%v; runner %s: %v", joined, r.AccountID(), err)
		}
	}
	return joined
}

// Count returns the number of live runners.
func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.runners)
}
