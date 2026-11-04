package live

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// This file implements the sanitized evidence pipeline (Phase 18 §27):
//
//   - Records carry only shape evidence (constructor names, counts, sizes,
//     hash prefixes, error classes, latencies, pts values).
//   - A redactor registered with every operator secret scrubs the serialized
//     record before it touches disk or stdout.
//   - Structural rules: message text, media bytes, and entity payloads are
//     NEVER recorded — only lengths and SHA-256 prefixes.

// Status values (§7).
const (
	StatusPass          = "PASS"
	StatusFail          = "FAIL"
	StatusBlocked       = "BLOCKED"
	StatusSkipped       = "SKIPPED"
	StatusNotObserved   = "NOT_OBSERVED"
	StatusNotApplicable = "NOT_APPLICABLE"
)

// Source values (§7) — every record names exactly one validation source.
const (
	SourceLive      = "LIVE"
	SourceFake      = "FAKE"
	SourceReference = "REFERENCE"
	SourceStatic    = "STATIC"
)

// Evidence is one sanitized result record.
type Evidence struct {
	Time   string         `json:"ts"`
	Suite  string         `json:"suite"`
	Op     string         `json:"op"`
	Status string         `json:"status"`
	Source string         `json:"source"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Redactor scrubs known secret material from any string. It is built from
// the operator configuration; a secret registered here can never appear in
// an evidence record even if a caller accidentally includes it.
type Redactor struct {
	mu      sync.Mutex
	secrets []namedSecret
}

type namedSecret struct {
	value string
	kind  string
}

// NewRedactor registers secret values with their classification. Empty
// values are ignored. Values shorter than 4 characters are not registered
// (redaction of a 1–3 char token is useless and would corrupt output); the
// operator is expected to pass real credentials.
func NewRedactor(secrets map[string]string) *Redactor {
	r := &Redactor{}
	for v, kind := range secrets {
		if len(v) >= 4 {
			r.secrets = append(r.secrets, namedSecret{value: v, kind: kind})
		}
	}
	return r
}

// Register adds one more secret (e.g. a session's auth key learned at
// runtime).
func (r *Redactor) Register(value, kind string) {
	if len(value) < 4 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.secrets = append(r.secrets, namedSecret{value: value, kind: kind})
}

// Redact replaces every registered secret with a typed placeholder.
func (r *Redactor) Redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec.value, "<"+sec.kind+">")
	}
	return s
}

// Recorder collects Evidence records, optionally persisting them as JSONL.
// Every record passes through the redactor before storage.
type Recorder struct {
	mu      sync.Mutex
	entries []Evidence
	w       *os.File
	red     *Redactor
}

// NewRecorder creates a recorder; when path is non-empty the records are
// appended as JSONL (redacted). The file is created with 0600 permissions.
func NewRecorder(path string, red *Redactor) (*Recorder, error) {
	r := &Recorder{red: red}
	if path == "" {
		return r, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	r.w = f
	return r, nil
}

// Record stores (and persists) one evidence record. Every string field is
// redacted BEFORE storage — in-memory entries and file lines are equally
// safe (a secret must not survive in either).
func (r *Recorder) Record(suite, op, status, source string, fields map[string]any) {
	safeFields := make(map[string]any, len(fields))
	for k, v := range fields {
		if str, ok := v.(string); ok {
			safeFields[k] = r.red.Redact(str)
		} else {
			safeFields[k] = v
		}
	}
	e := Evidence{
		Time:   time.Now().UTC().Format(time.RFC3339Nano),
		Suite:  suite,
		Op:     op,
		Status: status,
		Source: source,
		Fields: safeFields,
	}
	r.mu.Lock()
	r.entries = append(r.entries, e)
	line, err := json.Marshal(e)
	if err == nil && r.w != nil {
		safeLine := r.red.Redact(string(line))
		_, _ = r.w.WriteString(safeLine + "\n")
	}
	r.mu.Unlock()
}

// Entries returns a copy of all recorded evidence.
func (r *Recorder) Entries() []Evidence {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Evidence, len(r.entries))
	copy(out, r.entries)
	return out
}

// Count returns the number of records matching (optionally empty) filters.
func (r *Recorder) Count(status, source string) int {
	n := 0
	for _, e := range r.Entries() {
		if (status == "" || e.Status == status) && (source == "" || e.Source == source) {
			n++
		}
	}
	return n
}

// Summary renders the classification table used by the phase report.
func (r *Recorder) Summary() string {
	var b strings.Builder
	for _, e := range r.Entries() {
		fmt.Fprintf(&b, "%s / %s — %s.%s", e.Source, e.Status, e.Suite, e.Op)
		if len(e.Fields) > 0 {
			keys := make([]string, 0, len(e.Fields))
			for k := range e.Fields {
				keys = append(keys, k)
			}
			fmt.Fprintf(&b, " [%s]", strings.Join(keys, ","))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Close releases the evidence file, if any.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		err := r.w.Close()
		r.w = nil
		return err
	}
	return nil
}

// HashOf returns the SHA-256 hex prefix of arbitrary content — the ONLY
// sanctioned way to identify message text or media bytes in evidence
// (§27: prefer hashes/counts over raw content).
func HashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}
