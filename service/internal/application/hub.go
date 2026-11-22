// Package application hosts the account runtime (the heart of the
// service): the AccountRunner actor — one per bound account — with
// serialized mutations, concurrent bounded reads, an update fan-out hub
// with bounded per-subscriber queues, and session persistence
// integration (seal → store on every upstream persistence signal).
//
// Guarantees (tested):
//   - exactly one client session per account per process;
//   - mutations serialize per account; reads run concurrently up to a
//     bounded semaphore; a slow read/mutation never blocks shutdown;
//   - slow update subscribers are dropped (their buffer overflows) and
//     never block the account pipeline or other subscribers;
//   - session blobs are sealed before persistence and unsealed on load;
//   - duplicate runner creation is rejected deterministically.
package application

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// RunnerConfig bounds one account runtime.
type RunnerConfig struct {
	AccountID      domain.AccountID
	TenantID       string
	MutationQueue  int  // bounded mutation queue depth (default 64)
	ReadConcurrent int  // concurrent read semaphore (default 16)
	StreamBuffer   int  // per-subscriber event buffer (default 1024)
	AutoConnect    bool // connect during Start
}

func (c RunnerConfig) withDefaults() RunnerConfig {
	if c.MutationQueue <= 0 {
		c.MutationQueue = 64
	}
	if c.ReadConcurrent <= 0 {
		c.ReadConcurrent = 16
	}
	if c.StreamBuffer <= 0 {
		c.StreamBuffer = 1024
	}
	return c
}

// Runner owns one upstream client session.
type Runner struct {
	cfg    RunnerConfig
	factor ports.UpstreamFactory
	vault  ports.SessionVault
	repo   ports.SessionRepository

	mu      sync.Mutex
	session ports.ClientSession // nil until Start
	desired domain.DesiredState

	mutQ     chan mutationRequest
	readS    chan struct{}
	hub      *updateHub
	pumpCtx  context.Context
	pumpStop context.CancelFunc

	started atomic.Bool
	stopped atomic.Bool
	stopMu  sync.Mutex
}

type mutationRequest struct {
	op   func(ports.ClientSession) error
	done chan error
}

// NewRunner builds a runner (no I/O; Start performs the work).
func NewRunner(cfg RunnerConfig, factor ports.UpstreamFactory,
	vault ports.SessionVault, repo ports.SessionRepository) *Runner {
	cfg = cfg.withDefaults()
	return &Runner{
		cfg:    cfg,
		factor: factor,
		vault:  vault,
		repo:   repo,
		mutQ:   make(chan mutationRequest, cfg.MutationQueue),
		readS:  make(chan struct{}, cfg.ReadConcurrent),
	}
}

// AccountID returns the runner's account.
func (r *Runner) AccountID() domain.AccountID { return r.cfg.AccountID }

// Start loads the session (unseal → blob), constructs the client, wires
// the update subscription, and optionally connects. Idempotent under
// mutex; the second call is a no-op.
func (r *Runner) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started.Load() {
		return nil
	}

	var blob []byte
	if r.repo != nil {
		b, err := r.repo.Load(ctx, r.cfg.AccountID)
		switch {
		case err == nil:
			plain, err := r.vault.Unseal(ctx, b)
			if err != nil {
				return fmt.Errorf("runner %s: unseal session: %w", r.cfg.AccountID, err)
			}
			blob = plain
		case errors.Is(err, domain.ErrNotFound):
			// fresh account
		default:
			return fmt.Errorf("runner %s: load session: %w", r.cfg.AccountID, err)
		}
	}

	onPersist := func(raw []byte) error {
		if r.repo == nil {
			return nil
		}
		sealed, err := r.vault.Seal(ctx, raw)
		if err != nil {
			return fmt.Errorf("runner %s: seal session: %w", r.cfg.AccountID, err)
		}
		return r.repo.Store(ctx, r.cfg.AccountID, sealed)
	}

	sess, err := r.factor.CreateClient(ctx, ports.ClientSpec{
		AccountID:   r.cfg.AccountID,
		SessionBlob: blob,
		OnPersist:   onPersist,
	})
	if err != nil {
		return fmt.Errorf("runner %s: create client: %w", r.cfg.AccountID, err)
	}

	r.hub = newUpdateHub(r.cfg.StreamBuffer)
	if _, err := sess.SubscribeUpdates(ctx, func(ctx context.Context, ev domain.UpdateEvent) {
		r.hub.publish(ev)
	}); err != nil {
		return fmt.Errorf("runner %s: subscribe updates: %w", r.cfg.AccountID, err)
	}

	if r.cfg.AutoConnect {
		if err := sess.Connect(ctx); err != nil {
			// The client remains constructed (retryable via Connect RPC);
			// startup reports the failure honestly.
			return fmt.Errorf("runner %s: connect: %w", r.cfg.AccountID, err)
		}
		r.desired = domain.DesiredRunning
	}

	r.session = sess
	r.started.Store(true)

	// Mutation pump: one goroutine per runner, owned lifecycle. On stop it
	// drains the queue failing every pending request (no silent hangs).
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	r.pumpCtx, r.pumpStop = pumpCtx, pumpCancel
	go r.pump(pumpCtx)
	return nil
}

// pump services the mutation queue until the runner stops. The pump owns
// the serialization guarantee: exactly one mutation executes at a time.
func (r *Runner) pump(ctx context.Context) {
	for {
		select {
		case req := <-r.mutQ:
			r.mu.Lock()
			sess := r.session
			r.mu.Unlock()
			if sess == nil {
				req.done <- fmt.Errorf("%w: %s", domain.ErrAccountStopped, r.cfg.AccountID)
				continue
			}
			req.done <- req.op(sess)
		case <-ctx.Done():
			// Fail all pending requests — no silent hangs on shutdown.
			for {
				select {
				case req := <-r.mutQ:
					req.done <- fmt.Errorf("%w: %s (runner stopped)",
						domain.ErrAccountStopped, r.cfg.AccountID)
				default:
					return
				}
			}
		}
	}
}

// Stop closes the client session exactly once. Bounded by ctx; a blocked
// upstream close cannot prevent shutdown (the runner is marked stopped
// regardless).
func (r *Runner) Stop(ctx context.Context) error {
	r.stopMu.Lock()
	defer r.stopMu.Unlock()
	if r.stopped.Load() {
		return nil
	}
	if r.pumpStop != nil {
		r.pumpStop()
	}
	if r.hub != nil {
		r.hub.close()
	}
	r.mu.Lock()
	sess := r.session
	r.session = nil
	r.mu.Unlock()
	r.stopped.Store(true)

	if sess == nil {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- sess.Close(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// The close attempt continues in the background; the runner is
		// already marked stopped and will never reopen.
		return ctx.Err()
	}
}

// Mutate submits a state-changing operation to the account's serialized
// queue. Errors propagate verbatim from the operation. Rejection with
// ErrTooManyRequests occurs when the bounded queue is full.
func (r *Runner) Mutate(ctx context.Context, op func(ports.ClientSession) error) error {
	if _, err := r.currentSession(); err != nil {
		return err
	}
	req := mutationRequest{op: func(s ports.ClientSession) error { return op(s) }, done: make(chan error, 1)}
	select {
	case r.mutQ <- req:
	case <-ctx.Done():
		return ctx.Err()
	default:
		return fmt.Errorf("%w: mutation queue full for account %s",
			domain.ErrTooManyRequests, r.cfg.AccountID)
	}
	// The runner's pump drains mutQ; in S1-tests the pump runs inline via
	// drainQueue. See Manager.StartPumps.
	select {
	case err := <-req.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Read runs a read-only operation concurrently with other reads (bounded
// by the read semaphore). Reads never serialize behind mutations.
func (r *Runner) Read(ctx context.Context, fn func(ports.ClientSession) error) error {
	sess, err := r.currentSession()
	if err != nil {
		return err
	}
	select {
	case r.readS <- struct{}{}:
		defer func() { <-r.readS }()
	case <-ctx.Done():
		return ctx.Err()
	}
	return fn(sess)
}

func (r *Runner) currentSession() (ports.ClientSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started.Load() || r.session == nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrAccountStopped, r.cfg.AccountID)
	}
	return r.session, nil
}

// SessionAuthState reads the live authorization state (unauthorized when
// no session exists).
func (r *Runner) SessionAuthState() domain.AuthState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.session == nil {
		return domain.AuthUnauthorized
	}
	return r.session.AuthState()
}

// SessionConnected reports live upstream connectivity.
func (r *Runner) SessionConnected() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session != nil && r.session.Connected()
}

// Subscribe registers an update stream. The returned channel receives
// events until the subscriber is dropped (overflow) or the runner stops.
// dropReason is set when the subscriber is disconnected.
func (r *Runner) Subscribe(ctx context.Context) (<-chan domain.UpdateEvent, func(), error) {
	if r.hub == nil {
		return nil, nil, fmt.Errorf("%w: hub not initialized for %s",
			domain.ErrAccountStopped, r.cfg.AccountID)
	}
	return r.hub.subscribe()
}

// SetDesired records operator intent and reconciles the observed state:
// RUNNING → Connect (blocking, ctx-bounded); STOPPED → Disconnect.
// The observation snapshot reflects post-action state.
func (r *Runner) SetDesired(ctx context.Context, desired domain.DesiredState) error {
	r.mu.Lock()
	r.desired = desired
	sess := r.session
	r.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("%w: %s", domain.ErrAccountStopped, r.cfg.AccountID)
	}
	if desired == domain.DesiredRunning {
		if err := sess.Connect(ctx); err != nil {
			return translateRunnerErr(err)
		}
		return nil
	}
	return sess.Disconnect(ctx)
}

// Observation returns the current desired/observed snapshot.
func (r *Runner) Observation() domain.ConnectionObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	obs := domain.ConnectionObservation{Desired: r.desired}
	if r.session == nil || !r.session.Connected() {
		obs.Observed = domain.ObservedDisconnected
	} else if r.session.AuthState() == domain.AuthAuthorized {
		obs.Observed = domain.ObservedReady
	} else {
		obs.Observed = domain.ObservedConnected
	}
	obs.ChangedAt = time.Now()
	return obs
}

// translateRunnerErr wraps runner-level errors into the domain taxonomy.
func translateRunnerErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, domain.ErrUpstreamUnavailable) || errors.Is(err, domain.ErrUpstreamProtocol) ||
		errors.Is(err, domain.ErrUpstreamTimeout) || errors.Is(err, domain.ErrAmbiguousMutation) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err // already classified by the adapter
	}
	return fmt.Errorf("%w: %v", domain.ErrUpstreamUnavailable, err)
}
