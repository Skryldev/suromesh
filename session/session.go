// Package session implements the SuroMesh session model: identity/DC/auth
// state with deterministic persistence and a crash-safe file format.
//
// Design (docs/session.md is authoritative):
//
//	Session (interface)
//	├── Memory          — in-process state
//	├── File            — Memory + atomic versioned persistence (JSON v1)
//	└── StringSession   — SPlusthon wire-compatible compact form
//
// The layer deliberately separates LONG-LIVED authorization state (auth
// key, DC, address) from TRANSIENT transport state (session ID, sequence
// numbers) — the latter lives in the engine/transport and is never
// persisted.
package session

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Skryldev/suromesh/crypto"
)

// AuthState is the authorization lifecycle (docs/session.md §state):
//
//	NoKey → KeyExchanged → (Authorized | Unauthorized)
//
// KeyExchanged means a valid auth key exists (transport-level security);
// Authorized additionally means the account login (auth.signIn or the
// future bot token flow) succeeded. RPC-level 401s move Authorized →
// Unauthorized without touching the key.
type AuthState int

const (
	// StateNoKey: no auth key yet; the next connect performs the exchange.
	StateNoKey AuthState = iota
	// StateKeyExchanged: auth key established, not logged in.
	StateKeyExchanged
	// StateAuthorized: logged-in account with a valid auth key.
	StateAuthorized
	// StateUnauthorized: the server rejected the authorization (401-class);
	// the key itself may still be valid.
	StateUnauthorized
)

func (s AuthState) String() string {
	switch s {
	case StateNoKey:
		return "no_key"
	case StateKeyExchanged:
		return "key_exchanged"
	case StateAuthorized:
		return "authorized"
	case StateUnauthorized:
		return "unauthorized"
	}
	return "unknown"
}

// Session errors.
var (
	// ErrClosed is returned by operations on a closed session.
	ErrClosed = errors.New("session: closed")
	// ErrInvalidState reports an illegal lifecycle transition.
	ErrInvalidState = errors.New("session: invalid state transition")
)

// Session is the in-memory state container. All methods are safe for
// concurrent use.
type Session struct {
	mu        sync.RWMutex
	dcID      int
	addr      string
	port      int
	authKey   *crypto.AuthKey
	authState AuthState

	// Update state (Phase 10; reference sqlite update_state table,
	// sessions/sqlite.py:144-149 — persisted with the session).
	updState   *UpdateStateRecord
	chanStates map[int64]int32 // bounded; see chanStateLimit
	closed     bool
}

// New creates an empty session in StateNoKey.
func New() *Session { return &Session{} }

// NewWithKey creates a session from a loaded auth key (StateKeyExchanged).
func NewWithKey(key *crypto.AuthKey) *Session {
	return &Session{authKey: key, authState: StateKeyExchanged}
}

// DC returns the data-center ID, address, and port.
func (s *Session) DC() (dcID int, addr string, port int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dcID, s.addr, s.port
}

// SetDC updates DC routing information.
func (s *Session) SetDC(dcID int, addr string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.dcID, s.addr, s.port = dcID, addr, port
	return nil
}

// AuthKey returns the current key (nil when absent). The returned pointer
// aliases the session's key; treat it as read-only.
func (s *Session) AuthKey() *crypto.AuthKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authKey
}

// SetAuthKey installs an exchanged key (StateNoKey → StateKeyExchanged).
func (s *Session) SetAuthKey(key *crypto.AuthKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if key == nil || !key.HasValue {
		return fmt.Errorf("%w: empty key", ErrInvalidState)
	}
	if s.authKey != nil && s.authKey.KeyID != key.KeyID && s.authState == StateAuthorized {
		return fmt.Errorf("%w: key rotation while authorized", ErrInvalidState)
	}
	s.authKey = key
	if s.authState == StateNoKey {
		s.authState = StateKeyExchanged
	}
	return nil
}

// ClearAuthKey drops the key (DC migration, server-side key loss). The
// state returns to NoKey; authorization is implicitly revoked.
func (s *Session) ClearAuthKey() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.authKey = nil
	s.authState = StateNoKey
	return nil
}

// AuthState reports the authorization lifecycle state.
func (s *Session) AuthState() AuthState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authState
}

// UpdateStateRecord is the persisted account-level update state
// (pts/qts/date/seq — reference SessionState minus identity fields).
type UpdateStateRecord struct {
	Pts  int32
	Qts  int32
	Date int32
	Seq  int32
}

// ChannelUpdateRecord is one channel's persisted pts.
type ChannelUpdateRecord struct {
	ChannelID int64
	Pts       int32
}

// chanStateLimit bounds the per-channel state table (memory bound; the
// reference's SQLite table is unbounded — a long-lived client joining
// thousands of channels is the only realistic way to approach this).
const chanStateLimit = 4096

// SetUpdateState stores the account-level update state.
func (s *Session) SetUpdateState(r UpdateStateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	rec := r
	s.updState = &rec
	return nil
}

// UpdateState returns the stored update state (false when unknown).
func (s *Session) UpdateState() (UpdateStateRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.updState == nil {
		return UpdateStateRecord{}, false
	}
	return *s.updState, true
}

// SetChannelUpdateState stores one channel's pts (bounded map; beyond the
// limit the oldest-by-insertion entries are dropped — Go maps have no
// order, so eviction is arbitrary but always bounded).
func (s *Session) SetChannelUpdateState(channelID int64, pts int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.chanStates == nil {
		s.chanStates = make(map[int64]int32)
	}
	if len(s.chanStates) >= chanStateLimit {
		if _, ok := s.chanStates[channelID]; !ok {
			for k := range s.chanStates {
				delete(s.chanStates, k)
				break // bounded eviction; documented
			}
		}
	}
	s.chanStates[channelID] = pts
	return nil
}

// ChannelUpdateStates returns all stored channel states.
func (s *Session) ChannelUpdateStates() []ChannelUpdateRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ChannelUpdateRecord, 0, len(s.chanStates))
	for id, pts := range s.chanStates {
		out = append(out, ChannelUpdateRecord{ChannelID: id, Pts: pts})
	}
	return out
}

// SetAuthorized marks the account as logged in. Requires a key.
func (s *Session) SetAuthorized() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.authKey == nil {
		return fmt.Errorf("%w: authorized without a key", ErrInvalidState)
	}
	s.authState = StateAuthorized
	return nil
}

// SetUnauthorized marks server-side authorization rejection.
func (s *Session) SetUnauthorized() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.authState == StateAuthorized {
		s.authState = StateUnauthorized
	}
}

// sessionSnapshot is a consistent view of the persisted fields, taken
// under one lock hold (persistence layers must use it instead of reading
// the embedded fields directly — see FileSession.Save).
type sessionSnapshot struct {
	dcID       int
	addr       string
	port       int
	state      AuthState
	key        *crypto.AuthKey
	updState   *UpdateStateRecord
	chanStates map[int64]int32
}

func (s *Session) snapshot() sessionSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := sessionSnapshot{dcID: s.dcID, addr: s.addr, port: s.port,
		state: s.authState, key: s.authKey, chanStates: s.chanStates}
	if s.updState != nil {
		rec := *s.updState
		snap.updState = &rec
	}
	return snap
}

// Redacted renders a secret-free identity line for logs.
func (s *Session) Redacted() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keyInfo := "none"
	if s.authKey != nil {
		keyInfo = fmt.Sprintf("id=%016x", s.authKey.KeyID)
	}
	return fmt.Sprintf("session dc=%d addr=%s port=%d key(%s) auth=%s",
		s.dcID, s.addr, s.port, keyInfo, s.authState)
}

// Close releases the session. The auth key reference is dropped rather
// than zeroed in place: the returned AuthKey pointer is aliased by the
// RPC engine's encryption path (AuthKey doc: "treat it as read-only"),
// and in-place zeroing raced with concurrent reads (found by
// TestAuthSessionRestartLive under -race, Phase 14). Go cannot guarantee
// erasure of the bytes from memory — documented limitation either way;
// dropping the reference achieves the same practical posture without the
// race.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.closed = true
	// Drop the key AND the state label together: a post-close Save must
	// serialize a CONSISTENT keyless session (the loader rejects
	// key_exchanged without a key — Phase 15 concurrent save/close test).
	s.authKey = nil
	s.authState = StateNoKey
	return nil
}
