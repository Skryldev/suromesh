package live

import (
	"fmt"
	"sync"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/session"
)

// bootstrap.go: client construction with sanitized RPC tracing. The trace
// records METHOD NAMES, latencies and error classes only — never payloads,
// never identifiers beyond counts (Phase 18 §27).

// RPCTrace is the sanitized RPC observation log.
type RPCTrace struct {
	mu     sync.Mutex
	calls  []rpcCall
	redact *Redactor
}

type rpcCall struct {
	Method   string
	Duration time.Duration
	ErrClass string
}

// NewTrace creates an empty trace. errClass is derived with
// client.ClassifyError (secret-free by contract).
func NewTrace(red *Redactor) *RPCTrace {
	return &RPCTrace{redact: red}
}

// Hooks returns the client hook set wired to this trace. Additional
// per-suite hooks can be composed by the caller.
func (t *RPCTrace) Hooks() client.Hooks {
	return client.Hooks{
		OnRPCStart:    func(string) {},
		OnRPCComplete: t.record,
		OnRPCRetry: func(method string, attempt int, reason error) {
			t.mu.Lock()
			t.calls = append(t.calls, rpcCall{
				Method:   method + fmt.Sprintf("#retry%d", attempt),
				ErrClass: t.classify(reason),
			})
			t.mu.Unlock()
		},
	}
}

func (t *RPCTrace) record(method string, d time.Duration, err error) {
	t.mu.Lock()
	t.calls = append(t.calls, rpcCall{Method: method, Duration: d, ErrClass: t.classify(err)})
	t.mu.Unlock()
}

func (t *RPCTrace) classify(err error) string {
	if err == nil {
		return ""
	}
	return client.ClassifyError(err)
}

// Methods returns the recorded method names in order.
func (t *RPCTrace) Methods() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, len(t.calls))
	for i, c := range t.calls {
		out[i] = c.Method
	}
	return out
}

// Count returns how many times method was invoked (exact match).
func (t *RPCTrace) Count(method string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, c := range t.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// HasAny reports whether any recorded method name contains substr.
func (t *RPCTrace) HasAny(substr string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, c := range t.calls {
		if contains(c.Method, substr) {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// authRPCMethods are the method-name substrings whose presence on a
// connection proves an authentication RPC was issued (§9: a session-reuse
// connection must contain NONE of these).
var authRPCMethods = []string{"auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.exportAuthorization", "auth.importAuthorization"}

// HasAuthRPC reports whether the trace contains any authentication RPC.
func (t *RPCTrace) HasAuthRPC() bool {
	for _, m := range authRPCMethods {
		if t.HasAny(m) {
			return true
		}
	}
	return false
}

// Boot is the result of Bootstrap: the client, its file session, and the
// session file path (for cleanup only — never recorded).
type Boot struct {
	Client      *client.Client
	FileSess    *session.FileSession
	SessionPath string
}

// Bootstrap builds a client for the live service from cfg, registering the
// session's auth key (if any) with the redactor before anything else runs.
// The returned client is NOT connected.
func Bootstrap(cfg Config, red *Redactor, trace *RPCTrace) (*Boot, error) {
	path := cfg.Session
	if path == "" {
		f, err := tempSessionFile()
		if err != nil {
			return nil, err
		}
		path = f
	}
	fs, err := session.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("live: open session: %w", err)
	}
	if k := fs.AuthKey(); k != nil {
		red.Register(keyFingerprint(k), "authkey")
	}
	hooks := trace.Hooks()
	host := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	if cfg.Port == 0 { // host already carries the port (factory reuse)
		host = cfg.Host
	}
	c, err := client.New(client.Config{
		Session:    fs.Session,
		Saver:      fs,
		Host:       host,
		TLSEnabled: &cfg.TLS,
		Hooks:      hooks,
	})
	if err != nil {
		return nil, err
	}
	return &Boot{Client: c, FileSess: fs, SessionPath: path}, nil
}
