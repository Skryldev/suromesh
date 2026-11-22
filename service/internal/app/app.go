// Package app implements the SuroMesh Service bootstrap: the runtime
// state machine (Created → Starting → Running → Stopping → Stopped |
// Failed), signal wiring, and deterministic cleanup semantics.
//
// Guarantees (tested in app_test.go):
//   - startup failure stops every already-started resource (reverse
//     order) and lands the application in Failed;
//   - shutdown executes exactly once — repeated and concurrent Stop
//     calls observe the same completion;
//   - shutdown respects the caller's context deadline;
//   - no global mutable state: everything is injected through New.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/config"
)

// State is the runtime lifecycle state.
type State int32

const (
	StateCreated  State = 0
	StateStarting State = 1
	StateRunning  State = 2
	StateStopping State = 3
	StateStopped  State = 4
	StateFailed   State = 5
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateRunning:
		return "running"
	case StateStopping:
		return "stopping"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	default:
		return "created"
	}
}

// ErrInvalidState reports a lifecycle call that the current state does
// not permit (e.g. Stop before Run).
var ErrInvalidState = errors.New("app: invalid state transition")

// Resource is re-exported for bootstrap wiring convenience.
type Resource = application.Resource

// App is the service runtime. Construct with New; drive with Run (the
// blocking form) and Stop (the cooperative form).
type App struct {
	cfg       config.Config
	logger    *slog.Logger
	resources []Resource
	sup       *application.Supervisor

	mu       sync.Mutex
	state    State
	runErr   error
	stopping chan struct{} // closed exactly once when stop is requested
	done     chan struct{} // closed exactly once when a terminal state is reached
	stopOnce sync.Once
}

// New wires the application. No resource is started here.
func New(cfg config.Config, logger *slog.Logger, resources ...Resource) *App {
	return &App{
		cfg:       cfg,
		logger:    logger,
		resources: resources,
		sup:       application.NewSupervisor(resources...),
		state:     StateCreated,
		stopping:  make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// State returns the current lifecycle state.
func (a *App) State() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Run drives the full lifecycle: start everything, serve until ctx is
// cancelled or Stop is called, then shut down. It returns the terminal
// error (nil on clean Stopped). Run must be called at most once.
func (a *App) Run(ctx context.Context) error {
	if err := a.transition(ctx, StateStarting); err != nil {
		return err // transition failure: nothing started yet
	}
	a.logger.InfoContext(ctx, "starting resources",
		slog.Int("resources", len(a.resources)))

	startCtx, cancelStart := context.WithTimeout(ctx, 60*time.Second)
	defer cancelStart()
	if err := a.sup.StartAll(startCtx); err != nil {
		a.finish(StateFailed, fmt.Errorf("app: startup: %w", err))
		return a.runErr
	}

	a.mu.Lock()
	a.state = StateRunning
	a.mu.Unlock()
	a.logger.InfoContext(ctx, "service running",
		slog.Any("config", a.cfg))

	// Serve until stop is requested or the parent context ends.
	select {
	case <-a.stopping:
	case <-ctx.Done():
	}

	if err := a.shutdown(ctx); err != nil {
		a.finish(StateFailed, fmt.Errorf("app: shutdown: %w", err))
	} else {
		a.finish(StateStopped, nil)
	}
	return a.runErr
}

// Stop requests termination exactly once and waits for the terminal
// state, bounded by ctx. Concurrent callers wait for the same
// completion and receive the same result. Stop before Run fails with
// ErrInvalidState (nothing to stop — deterministic).
func (a *App) Stop(ctx context.Context) error {
	a.mu.Lock()
	state := a.state
	a.mu.Unlock()
	switch state {
	case StateCreated:
		return ErrInvalidState
	case StateStopped, StateFailed:
		return a.runErr // already terminal: same result, no second stop
	case StateRunning, StateStarting, StateStopping:
		// request and wait below
	}
	a.stopOnce.Do(func() { close(a.stopping) })
	select {
	case <-a.done:
		return a.runErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shutdown performs the Stopping phase: reverse-order resource stop
// under the configured budget, further bounded by the caller's ctx.
func (a *App) shutdown(parent context.Context) error {
	a.mu.Lock()
	a.state = StateStopping
	a.mu.Unlock()
	a.logger.InfoContext(parent, "stopping resources")

	budget := a.cfg.Shutdown.Timeout
	stopCtx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	// Also honor the parent's remaining budget if it is shorter.
	go func() {
		<-parent.Done()
		cancel()
	}()
	return a.sup.StopAll(stopCtx)
}

func (a *App) transition(ctx context.Context, target State) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.state {
	case StateCreated:
		if target != StateStarting {
			return fmt.Errorf("%w: %s → %s", ErrInvalidState, a.state, target)
		}
		a.state = StateStarting
		return nil
	default:
		return fmt.Errorf("%w: cannot enter %s from %s", ErrInvalidState, target, a.state)
	}
}

// finish records the terminal state exactly once.
func (a *App) finish(state State, err error) {
	a.mu.Lock()
	a.state = state
	a.runErr = err
	a.mu.Unlock()
	if err != nil {
		a.logger.Error("application finished with error", slog.String("error", err.Error()))
	} else {
		a.logger.Info("application stopped cleanly")
	}
	a.stopOnce.Do(func() { close(a.stopping) })
	close(a.done)
}
