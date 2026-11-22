package domain_test

// Dependency-boundary check (Phase 25 §12): the domain package must not
// transitively import the SDK, protobuf, gRPC, or infrastructure
// implementations. Verified with `go list -deps` — the authoritative
// import graph.

import (
	"os/exec"
	"strings"
	"testing"
)

var forbiddenInDomain = []string{
	"github.com/Skryldev/suromesh/client",
	"github.com/Skryldev/suromesh/session",
	"github.com/Skryldev/suromesh/transport",
	"github.com/Skryldev/suromesh/internal/",
	"google.golang.org/grpc",
	"google.golang.org/protobuf",
}

func TestDomainDependencyBoundary(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/Skryldev/suromesh/service/internal/domain").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, out)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		for _, forbidden := range forbiddenInDomain {
			if strings.Contains(dep, forbidden) {
				t.Errorf("domain imports forbidden dependency %q", dep)
			}
		}
	}
}
