// Package live implements the opt-in live-validation harness for the real
// Soroush Plus service (Phase 18).
//
// ABSOLUTE RULES (Phase 18 spec §2, §5, §6):
//
//   - Live tests NEVER run accidentally: every live test requires
//     SP_SDK_LIVE=1 plus the environment-provided configuration it needs,
//     and fails closed (t.Skip ⇒ BLOCKED) when anything is absent.
//   - No credential ever enters source control, logs, evidence records, or
//     test artifacts. Secrets are read from the environment (or a file
//     named by the environment) and registered with the redactor.
//   - Every result carries one of PASS/FAIL/BLOCKED/SKIPPED/NOT_OBSERVED/
//     NOT_APPLICABLE and one of LIVE/FAKE/REFERENCE/STATIC — never mixed.
//
// A missing configuration is a successful, reported outcome:
//
//	LIVE / BLOCKED — REAL CREDENTIALS REQUIRED
package live

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the operator-provided live configuration. Zero-value fields
// mean "not provided", which the guards translate into BLOCKED skips.
type Config struct {
	Host    string // SP_LIVE_HOST (default im-server.splus.ir)
	Port    int    // SP_LIVE_PORT (default 443)
	TLS     bool   // SP_LIVE_TLS=0 disables TLS (default on)
	Session string // SP_LIVE_SESSION — session file path (persistent)

	Phone    string // SP_LIVE_PHONE or file via SP_LIVE_PHONE_FILE
	Code     string // SP_LIVE_CODE or file via SP_LIVE_CODE_FILE
	Password string // SP_LIVE_PASSWORD or file via SP_LIVE_PASSWORD_FILE

	Peer     string // SP_LIVE_PEER — test peer username (no @), read-only tests
	PeerKind string // SP_LIVE_PEER_KIND — "user"|"chat"|"channel" (default user)

	Mutations     bool          // SP_LIVE_MUTATIONS=1 — write operations opt-in
	Media         bool          // SP_LIVE_MEDIA=1 — media transfer opt-in
	BadCode       bool          // SP_LIVE_BAD_CODE=1 — one invalid-code probe
	IncomingWait  time.Duration // SP_LIVE_INCOMING_WAIT — operator-assisted incoming update window
	Soak          time.Duration // SP_LIVE_SOAK_MINUTES — bounded soak (0 = skip)
	EvidencePath  string        // SP_LIVE_EVIDENCE — JSONL evidence file
	KeepArtifacts bool          // SP_LIVE_KEEP=1 — skip cleanup

	// Areas is the explicit activation list (SP_LIVE_AREAS). NOTHING runs
	// unless the operator names the area — SP_SDK_LIVE=1 alone is never
	// sufficient (fail closed, §5).
	Areas map[string]bool
}

// AllAreas is the full activation vocabulary.
var AllAreas = []string{"connect", "auth", "session", "peers", "dialogs",
	"mutations", "updates", "media", "reconnect", "errors", "soak"}

// Load reads the configuration. enabled is false unless SP_SDK_LIVE=1 —
// in that case every live test must report BLOCKED and skip.
func Load() (Config, bool) {
	if os.Getenv("SP_SDK_LIVE") != "1" {
		return Config{}, false
	}
	cfg := Config{
		Host:          envStr("SP_LIVE_HOST", "im-server.splus.ir"),
		Port:          envInt("SP_LIVE_PORT", 443),
		TLS:           os.Getenv("SP_LIVE_TLS") != "0",
		Session:       os.Getenv("SP_LIVE_SESSION"),
		Phone:         readSecret("SP_LIVE_PHONE", "SP_LIVE_PHONE_FILE"),
		Code:          readSecret("SP_LIVE_CODE", "SP_LIVE_CODE_FILE"),
		Password:      readSecret("SP_LIVE_PASSWORD", "SP_LIVE_PASSWORD_FILE"),
		Peer:          strings.TrimPrefix(os.Getenv("SP_LIVE_PEER"), "@"),
		PeerKind:      strings.ToLower(envStr("SP_LIVE_PEER_KIND", "user")),
		Mutations:     os.Getenv("SP_LIVE_MUTATIONS") == "1",
		Media:         os.Getenv("SP_LIVE_MEDIA") == "1",
		BadCode:       os.Getenv("SP_LIVE_BAD_CODE") == "1",
		IncomingWait:  envDur("SP_LIVE_INCOMING_WAIT", 0),
		Soak:          time.Duration(envInt("SP_LIVE_SOAK_MINUTES", 0)) * time.Minute,
		EvidencePath:  os.Getenv("SP_LIVE_EVIDENCE"),
		KeepArtifacts: os.Getenv("SP_LIVE_KEEP") == "1",
	}
	if cfg.Soak > 360*time.Minute {
		cfg.Soak = 360 * time.Minute // §28: 1–6 h hard cap
	}
	cfg.Areas = map[string]bool{}
	for _, a := range strings.Split(os.Getenv("SP_LIVE_AREAS"), ",") {
		a = strings.TrimSpace(strings.ToLower(a))
		if a != "" {
			cfg.Areas[a] = true
		}
	}
	return cfg, true
}

// AreaEnabled reports whether the operator activated the area.
func (c Config) AreaEnabled(area string) bool { return c.Areas[area] }

// AuthorizedSessionAvailable reports whether an account-bearing test can
// run: it needs an operator-provided session file that a prior auth run
// (or the operator) made authorized.
func (c Config) AuthorizedSessionAvailable() bool { return c.Session != "" }

// SecretNames lists every configuration field that holds a secret, with the
// env/file variable names it may have come from — used to build the
// redactor's registry so the SOURCE of a secret is tracked too.
func (c Config) SecretNames() map[string]string {
	m := map[string]string{}
	if c.Phone != "" {
		m[c.Phone] = "phone"
	}
	if c.Code != "" {
		m[c.Code] = "otp"
	}
	if c.Password != "" {
		m[c.Password] = "password"
	}
	return m
}

// AuthRequirementsMet reports whether the auth flow can run without
// wasting a real SMS: the operator must provide BOTH the phone and the
// code before the harness sends anything (fail closed — no code means no
// SendCode either).
func (c Config) AuthRequirementsMet() bool {
	return c.Phone != "" && c.Code != ""
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return def
}

// readSecret reads a secret from an env var or from the file the companion
// *_FILE env var names. The file is read once and its content trimmed —
// the path itself is never recorded anywhere.
func readSecret(envName, fileEnvName string) string {
	if v := os.Getenv(envName); v != "" {
		return strings.TrimSpace(v)
	}
	if p := os.Getenv(fileEnvName); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return ""
}
