package compatibility_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tests/vectors"
)

type malformedSessionCase struct {
	Name        string `json:"name"`
	Session     string `json:"session"`
	PythonError string `json:"python_error,omitempty"`
	DC          int    `json:"dc,omitempty"`
	Address     string `json:"address,omitempty"`
	Port        int    `json:"port,omitempty"`
	Key         string `json:"key,omitempty"`
}

// TestStringSessionMalformedMatchesReference replays the Python oracle:
// every malformed input must be rejected in Go exactly as the reference
// rejects it, and every valid input must parse to identical fields.
func TestStringSessionMalformedMatchesReference(t *testing.T) {
	f := mustLoad(t, "string_session_malformed.json")
	var cases []malformedSessionCase
	if err := json.Unmarshal(f.Cases, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			ss, err := session.ParseStringSession(c.Session)
			if c.PythonError != "" {
				// Reference rejected it: Go must too.
				if err == nil {
					t.Fatalf("Go accepted what Python rejected (%s): %+v", c.PythonError, ss)
				}
				return
			}
			if err != nil {
				t.Fatalf("Go rejected what Python accepted: %v", err)
			}
			if ss.DCID != c.DC || ss.ServerAddr != c.Address || ss.Port != c.Port {
				t.Fatalf("fields: %+v want dc=%d addr=%s port=%d", ss, c.DC, c.Address, c.Port)
			}
			if c.Key == "" {
				// Empty key expectation: the session must be keyless.
				if ss.HasAuthKey {
					t.Fatal("expected keyless session")
				}
			} else if want := vectors.Hex(c.Key); string(ss.AuthKeyValue[:]) != string(want) {
				t.Fatal("key mismatch")
			}
			// Round trip mirrors the reference: a zero-key session
			// serializes to "" (save() returns '' without a key), which
			// parses back as a fresh empty session.
			if !ss.HasAuthKey {
				if ss.String() != "" {
					t.Fatal("keyless session must serialize to empty")
				}
				return
			}
			re, err := session.ParseStringSession(ss.String())
			if err != nil {
				t.Fatalf("re-parse: %v", err)
			}
			if re.String() != ss.String() {
				t.Fatal("re-serialization drift")
			}
		})
	}
}

// TestStringSessionFuzzSeam keeps the decoder hostile-input guarantees
// visible at the compatibility layer.
func TestStringSessionFuzzSeam(t *testing.T) {
	// "" is legal (fresh session); everything else malformed is rejected.
	inputs := []string{"1", "2AAAA", "1AAAA", "1AAAA0000", "1AgAA", "1!!!invalid-base64!!!"}
	for _, s := range inputs {
		if _, err := session.ParseStringSession(s); err == nil {
			t.Fatalf("malformed input %q accepted", s)
		}
	}
	if ss, err := session.ParseStringSession(""); err != nil || ss.HasAuthKey {
		t.Fatal("empty session semantics broken")
	}
	// A valid-format session with a zero key parses with HasAuthKey=false
	// (reference `if any(key)` semantics) and serializes to "".
	ss, err := session.ParseStringSession(
		"1AgABQQBQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==")
	if err != nil {
		t.Fatal(err)
	}
	if ss.HasAuthKey || ss.String() != "" {
		t.Fatal("zero-key session semantics violated")
	}
	_ = errors.Is
}
