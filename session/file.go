package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Skryldev/suromesh/crypto"
)

// File format v1 (docs/session.md §format):
//
//	magic  "SUROMESH"          (8 bytes)
//	u32    jsonLen
//	json   {"v":1,"dc":…,"addr":…,"port":…,"auth":hex-or-null,"state":…}
//	u64    hmac tag (SHA-256 over magic||len||json, key = magic)
//
// Deterministic: identical state serializes identically. Corruption is
// detected by magic, length bounds, JSON validity, and the HMAC tag — a
// corrupted file loads as ErrCorrupt, never as partial state.
//
// Persistence policy: write temp file in the same directory → fsync →
// atomic rename. A crash mid-write leaves the old file intact (rename is
// atomic on POSIX and Windows for practical purposes).
//
// Locking decision: the file is written atomically via rename, so readers
// always observe a complete old-or-new document; multi-process concurrent
// writers last-writer-wins by design (the session is per-process state).
// An advisory lock file adds failure modes (stale locks after crashes)
// without protecting anything rename does not; the decision is to NOT
// lock, and to document that two processes sharing one session file will
// overwrite each other's key rotations. docs/session.md records this.

// ErrCorrupt is returned for any unparsable/failed-integrity session file.
var ErrCorrupt = errors.New("session: corrupt session file")

var (
	fileMagic   = []byte("SUROMESH")
	fileHMACKey = []byte("SUROMESH") // integrity (accident detection), not secrecy
)

// fileSession is Memory-backed persistence.
type FileSession struct {
	*Session
	path string

	mu      sync.Mutex
	writing bool
}

// OpenFile loads a session file (creating an empty session when missing)
// and returns a session bound to that path.
func OpenFile(path string) (*FileSession, error) {
	fs := &FileSession{Session: New(), path: path}
	b, err := readWithRetry(path)
	if errors.Is(err, os.ErrNotExist) {
		return fs, nil // fresh session
	}
	if err != nil {
		return nil, fmt.Errorf("session: read: %w", err)
	}
	if len(b) == 0 {
		return fs, nil // empty file: treat as fresh
	}
	if err := fs.decode(b); err != nil {
		return nil, err
	}
	return fs, nil
}

type fileDoc struct {
	V     int    `json:"v"`
	DC    int    `json:"dc"`
	Addr  string `json:"addr"`
	Port  int    `json:"port"`
	Auth  string `json:"auth,omitempty"` // hex of the 256-byte key
	State string `json:"state"`

	// Phase 10: persisted update state (additive, optional — V1 files
	// without these fields load identically to before).
	Upd   *updStateDoc   `json:"upd,omitempty"`
	Chans []chanStateDoc `json:"chans,omitempty"`
}

type updStateDoc struct {
	Pts  int32 `json:"pts"`
	Qts  int32 `json:"qts"`
	Date int32 `json:"date"`
	Seq  int32 `json:"seq"`
}

type chanStateDoc struct {
	ID  int64 `json:"id"`
	Pts int32 `json:"pts"`
}

func (fs *FileSession) decode(b []byte) error {
	if len(b) < 8+4+2 || !bytesEqual(b[:8], fileMagic) {
		return fmt.Errorf("%w: bad magic", ErrCorrupt)
	}
	jsonLen := int(binary.LittleEndian.Uint32(b[8:12]))
	if jsonLen <= 0 || 12+jsonLen+8 > len(b) {
		return fmt.Errorf("%w: bad length %d", ErrCorrupt, jsonLen)
	}
	body := b[12 : 12+jsonLen]
	tag := b[12+jsonLen:]

	want := fileHMAC(body)
	if !hmac.Equal(tag, want) {
		return fmt.Errorf("%w: hmac mismatch", ErrCorrupt)
	}

	var doc fileDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("%w: json: %v", ErrCorrupt, err)
	}
	if doc.V != 1 {
		return fmt.Errorf("%w: version %d", ErrCorrupt, doc.V)
	}
	if doc.Port < 0 || doc.Port > 65535 || len(doc.Addr) > 255 {
		return fmt.Errorf("%w: dc metadata out of range", ErrCorrupt)
	}

	fs.dcID, fs.addr, fs.port = doc.DC, doc.Addr, doc.Port
	switch doc.State {
	case "", "no_key":
		fs.authState = StateNoKey
	case "key_exchanged":
		fs.authState = StateKeyExchanged
	case "authorized":
		fs.authState = StateAuthorized
	case "unauthorized":
		fs.authState = StateUnauthorized
	default:
		return fmt.Errorf("%w: state %q", ErrCorrupt, doc.State)
	}
	if doc.Auth != "" {
		keyBytes := decodeHexStrict(doc.Auth)
		if len(keyBytes) != 256 {
			return fmt.Errorf("%w: auth key length %d", ErrCorrupt, len(keyBytes))
		}
		var kb [256]byte
		copy(kb[:], keyBytes)
		fs.authKey = crypto.NewAuthKey(kb)
	}
	if fs.authKey == nil && (fs.authState == StateAuthorized || fs.authState == StateKeyExchanged) {
		return fmt.Errorf("%w: state %q without a key", ErrCorrupt, doc.State)
	}
	if doc.Upd != nil {
		fs.updState = &UpdateStateRecord{Pts: doc.Upd.Pts, Qts: doc.Upd.Qts,
			Date: doc.Upd.Date, Seq: doc.Upd.Seq}
	}
	if len(doc.Chans) > 0 {
		fs.chanStates = make(map[int64]int32, len(doc.Chans))
		for _, c := range doc.Chans {
			fs.chanStates[c.ID] = c.Pts
		}
	}
	return nil
}

// Save persists the session atomically: temp file in the same directory,
// fsync, rename. Mode 0600 on Unix (documented platform note: Windows
// ignores the Unix permission bits; ACL tightening is out of scope).
func (fs *FileSession) Save() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.writing {
		return ErrClosed
	}

	// Snapshot under the Session's own lock: mutators (SetAuthorized,
	// SetAuthKey, SetDC, ClearAuthKey) write those fields under s.mu, and
	// this method previously read them under fs.mu only — a data race
	// found by the Phase 9 concurrent save/load stress test. The returned
	// key pointer aliases the session's (read-only by convention).
	snap := fs.snapshot()
	doc := fileDoc{V: 1, DC: snap.dcID, Addr: snap.addr, Port: snap.port}
	switch snap.state {
	case StateNoKey:
		doc.State = "no_key"
	case StateKeyExchanged:
		doc.State = "key_exchanged"
	case StateAuthorized:
		doc.State = "authorized"
	case StateUnauthorized:
		doc.State = "unauthorized"
	}
	if snap.key != nil {
		doc.Auth = encodeHex(snap.key.Key[:])
	}
	if snap.updState != nil {
		doc.Upd = &updStateDoc{Pts: snap.updState.Pts, Qts: snap.updState.Qts,
			Date: snap.updState.Date, Seq: snap.updState.Seq}
	}
	if len(snap.chanStates) > 0 {
		doc.Chans = make([]chanStateDoc, 0, len(snap.chanStates))
		for id, pts := range snap.chanStates {
			doc.Chans = append(doc.Chans, chanStateDoc{ID: id, Pts: pts})
		}
	}
	body, err := json.Marshal(&doc)
	if err != nil {
		return err
	}

	out := make([]byte, 0, 12+len(body)+8)
	out = append(out, fileMagic...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	out = append(out, body...)
	out = append(out, fileHMAC(body)...)

	dir := filepath.Dir(fs.path)
	tmp, err := os.CreateTemp(dir, ".suromesh-session-*")
	if err != nil {
		return fmt.Errorf("session: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op after successful rename
	}()

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("session: chmod: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		return fmt.Errorf("session: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("session: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Windows: renaming onto a path another handle has open for reading
	// fails with a sharing violation. Retry briefly — readers finish in
	// microseconds; POSIX renames atomically on the first attempt.
	var renameErr error
	for attempt := 0; attempt < 10; attempt++ {
		if renameErr = os.Rename(tmpName, fs.path); renameErr == nil {
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return fmt.Errorf("session: rename: %w", renameErr)
}

// readWithRetry re-reads through transient Windows sharing violations
// ("being used by another process") and access-denied windows ("Access is
// denied") that occur while a concurrent Save is between CreateTemp and
// Rename. The bound keeps worst-case latency at ~20 ms; on POSIX the first
// attempt always succeeds (ErrNotExist passes through immediately).
func readWithRetry(path string) ([]byte, error) {
	var b []byte
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		b, err = os.ReadFile(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return b, err
		}
		var pe *os.PathError
		if !errors.As(err, &pe) {
			return b, err // non-FS error: surface immediately
		}
		time.Sleep(2 * time.Millisecond)
	}
	return b, err
}

// Delete removes the session file (log out / reset).
func (fs *FileSession) Delete() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	err := os.Remove(fs.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func fileHMAC(body []byte) []byte {
	m := hmac.New(sha256.New, fileHMACKey)
	m.Write(fileMagic)
	m.Write(body)
	return m.Sum(nil)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const hexDigits = "0123456789abcdef"

func encodeHex(b []byte) string {
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&15]
	}
	return string(out)
}

func decodeHexStrict(s string) []byte {
	if len(s)%2 != 0 {
		return nil
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(s); i += 2 {
		hi := hexVal(s[i])
		lo := hexVal(s[i+1])
		if hi < 0 || lo < 0 {
			return nil
		}
		out[i/2] = byte(hi<<4 | lo)
	}
	return out
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
