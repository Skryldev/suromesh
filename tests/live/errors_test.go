package live

import (
	"context"
	"testing"
	"time"
)

// TestLiveErrorMatrix (§24): safe, bounded error probes — the
// never-fabricate invariant and observed server error classes. No
// identifiers are probed beyond non-existent ones.
func TestLiveErrorMatrix(t *testing.T) {
	s := newLiveSuite(t, "errors")
	requireConnect(t, s)
	requireAuthorized(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := s.OpErrorMatrix(ctx); err != nil {
		t.Fatalf("error matrix: %v", err)
	}
}
