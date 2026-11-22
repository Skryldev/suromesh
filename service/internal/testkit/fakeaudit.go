package testkit

import (
	"context"
	"sync"

	"github.com/Skryldev/suromesh/service/internal/ports"
)

// FakeAudit is an in-memory AuditRecorder (deterministic test double).
type FakeAudit struct {
	mu     sync.Mutex
	events []ports.AuditEvent
}

func (f *FakeAudit) Record(ctx context.Context, e ports.AuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

// Events returns the recorded events (test assertion aid).
func (f *FakeAudit) Events() []ports.AuditEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ports.AuditEvent, len(f.events))
	copy(out, f.events)
	return out
}

// CountByAction counts events with the given action.
func (f *FakeAudit) CountByAction(action string) int {
	n := 0
	for _, e := range f.Events() {
		if e.Action == action {
			n++
		}
	}
	return n
}
