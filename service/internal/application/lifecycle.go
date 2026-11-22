// Package application hosts the ordered resource-lifecycle engine used
// by the bootstrap (and later by account runners): named resources with
// Start/Stop, started in declaration order, stopped in reverse, with
// failure cleanup that only touches already-started resources and
// per-resource stop budgets derived from the caller's deadline.
package application

import (
	"context"
	"errors"
	"fmt"
)

// Resource is one startable/stoppable unit of the service (gateway,
// listeners, runners...). Start must be idempotent-safe under the
// supervisor: it is called at most once per Run. Stop must tolerate
// being called even when Start failed mid-way — implementations should
// treat Stop as best-effort cleanup.
type Resource interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Supervisor owns an ordered set of resources.
type Supervisor struct {
	resources []Resource
	started   int // count of successfully started resources (prefix)
}

func NewSupervisor(resources ...Resource) *Supervisor {
	return &Supervisor{resources: resources}
}

// StartAll starts resources in order. On the first failure it stops the
// already-started prefix in reverse (best-effort; the first stop error
// is joined) and returns the start error wrapped with the failing
// resource name.
func (s *Supervisor) StartAll(ctx context.Context) error {
	for i, r := range s.resources {
		if err := r.Start(ctx); err != nil {
			s.started = i
			startErr := fmt.Errorf("resource %s: %w", r.Name(), err)
			if stopErr := s.StopAll(ctx); stopErr != nil {
				return errors.Join(startErr, stopErr)
			}
			return startErr
		}
	}
	s.started = len(s.resources)
	return nil
}

// StopAll stops started resources in reverse declaration order. Every
// started resource gets a Stop attempt even if an earlier one fails
// (all stop errors are joined). Stopping respects the caller's context
// budget: implementations must observe ctx; the supervisor does not
// invent its own timeout.
func (s *Supervisor) StopAll(ctx context.Context) error {
	var errs []error
	for i := s.started - 1; i >= 0; i-- {
		if err := s.resources[i].Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("resource %s: %w", s.resources[i].Name(), err))
		}
	}
	s.started = 0
	return errors.Join(errs...)
}

// StartedNames returns the names of currently started resources in
// start order (test/diagnostics aid).
func (s *Supervisor) StartedNames() []string {
	out := make([]string, 0, s.started)
	for i := 0; i < s.started; i++ {
		out = append(out, s.resources[i].Name())
	}
	return out
}
