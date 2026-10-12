package live

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Skryldev/suromesh/crypto"
)

// util.go: small shared helpers (session files, hashing sinks, timeouts).

func tempSessionFile() (string, error) {
	dir, err := os.MkdirTemp("", "suromesh-live")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "live.session"), nil
}

func removeFile(path string) error { return os.Remove(path) }

func itoa(n int) string { return strconv.Itoa(n) }

// keyFingerprint derives a NON-SECRET stable identifier for an auth key so
// evidence can state "same key across reconnects" without ever recording
// key material. FNV-1a over the raw bytes is sufficient — the fingerprint
// of a 256-byte secret is not useful to an attacker (it is not the key)
// and is additionally registered as a redactable token.
func keyFingerprint(k *crypto.AuthKey) string {
	if k == nil || !k.HasValue {
		return "none"
	}
	h := fnv.New64a()
	_, _ = h.Write(k.Key[:])
	return fmt.Sprintf("fnv64:%016x", h.Sum64())
}

// sha256Sink is an io.Writer that hashes everything written to it — the
// media round-trip never materializes the downloaded file (§20: preserve
// the O(part_size) memory contract).
type sha256Sink struct {
	h hash.Hash
	n int64
}

func newSHA256Sink() *sha256Sink { return &sha256Sink{h: sha256.New()} }

func (s *sha256Sink) Write(p []byte) (int, error) {
	n, err := s.h.Write(p)
	s.n += int64(n)
	return n, err
}

func (s *sha256Sink) Sum() string  { return "sha256:" + fmt.Sprintf("%x", s.h.Sum(nil)) }
func (s *sha256Sink) Bytes() int64 { return s.n }

// defaultTimeout bounds every live RPC sequence that does not bring its
// own deadline.
const defaultTimeout = 30 * time.Second

// liveConnectTimeout budgets the real-service connect. The measured
// high-latency path to im-server.splus.ir needs ~40 s for the WebSocket
// upgrade alone (Phase 21 live probe: HTTP 101 in 39.3 s), so the whole
// connect (WS handshake + key exchange + init) gets 180 s — bounded, but
// realistic for this link.
const liveConnectTimeout = 180 * time.Second

// describePeer renders the shape-safe form of a resolved peer for evidence
// (kind + numeric id only — usernames are redacted at the recorder level
// when registered; ids are shape evidence, §27).
