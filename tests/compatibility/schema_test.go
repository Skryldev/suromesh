package compatibility_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSchemaFilesMatchReference guards the canonical schema copies against
// drift from their recorded provenance.
//
// Since Phase 21 the reference tree is a local-only behavioral oracle
// (MUST-NOT-RELEASE, git-excluded), so the comparison target is the
// committed provenance snapshot in tests/testdata/schema_reference/ — a
// byte-identical copy of the reference's generator data captured when the
// schema was first adopted (Phase 1; cmp-verified against the reference in
// Phases 19–21). Any intentional schema change must update BOTH tl/schema
// and this snapshot in the same commit, making drift a visible, deliberate
// event instead of a silent one. No test requires the reference tree.
func TestSchemaFilesMatchReference(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve paths")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	for _, name := range []string{"api.tl", "mtproto.tl"} {
		local, err := os.ReadFile(filepath.Join(root, "tl", "schema", name))
		if err != nil {
			t.Fatalf("read local schema: %v", err)
		}
		snapshot, err := os.ReadFile(filepath.Join(root, "tests", "testdata", "schema_reference", name))
		if err != nil {
			t.Fatalf("read provenance snapshot: %v", err)
		}
		if !bytes.Equal(local, snapshot) {
			t.Fatalf("%s drifted from its recorded provenance (update tl/schema and tests/testdata/schema_reference together)", name)
		}
	}
}
