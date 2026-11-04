package live

import (
	"os"
)

// cleanup.go: deterministic artifact cleanup (§5). Message cleanup lives on
// Suite.Cleanup (it needs the client); this file handles filesystem
// artifacts and the evidence recorder.

// CleanupSessionFile removes a temp session file unless the operator asked
// to keep artifacts. The session file contains the auth key — it must not
// survive the test run unless explicitly retained for a follow-up run.
func CleanupSessionFile(path string, keep bool) {
	if path == "" || keep {
		return
	}
	_ = os.Remove(path)
}

// CleanupRecorder closes and optionally removes the evidence file.
func CleanupRecorder(r *Recorder, keep bool) {
	if r == nil {
		return
	}
	_ = r.Close()
}

// boundedCtx returns a context with the given timeout, cancelled at the
// end of fn — helper for the per-area live tests.
