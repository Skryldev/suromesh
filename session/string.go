// Package session implements SPlusthon-compatible session persistence.
// Phase 2 provides StringSession, byte-compatible with the reference
// implementation (sessions/string.py) in both its v1 layouts.
package session

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// StringSession stores just enough state to reconnect and re-authenticate
// without re-sending the login code: DC ID, server address, port, and the
// 256-byte authorization key.
type StringSession struct {
	DCID         int
	ServerAddr   string
	Port         int
	AuthKeyValue [256]byte
	HasAuthKey   bool
}

// CurrentVersion mirrors sessions/string.py CURRENT_VERSION ('1').
const CurrentVersion = '1'

// StringSession errors.
var (
	ErrNotAStringSession = errors.New("session: not a valid string session")
	ErrTruncated         = errors.New("session: truncated string session payload")
)

// ParseStringSession decodes a version-1 string session in the current
// layout:
//
//	dc_id:u8 | addr_len:u16be | addr | port:u16be | key:256
//
// The reference implementation also has a "legacy 352-byte" branch, but it is
// dead code there: the guard requires 352 bytes while its struct.unpack
// needs 263, so every legacy payload raises struct.error. Any payload that
// does not fit the current layout therefore yields an error here too,
// matching the reference's observable behavior.
func ParseStringSession(s string) (*StringSession, error) {
	if len(s) == 0 {
		// Reference behavior: an empty string is falsy, so the parse block
		// is skipped entirely — the result is a fresh empty session
		// (dc=0, no address, no key).
		return &StringSession{}, nil
	}
	if s[0] != CurrentVersion {
		return nil, ErrNotAStringSession
	}
	data, err := base64.URLEncoding.DecodeString(s[1:])
	if err != nil {
		// Python's base64 tolerates stripped padding; retry raw.
		data, err = base64.RawURLEncoding.DecodeString(s[1:])
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotAStringSession, err)
		}
	}
	if len(data) < 3 {
		return nil, ErrTruncated
	}
	ss := &StringSession{}
	addrLen := int(binary.BigEndian.Uint16(data[1:3]))
	need := 3 + addrLen + 2 + 256
	if len(data) < need {
		return nil, ErrTruncated
	}
	ss.DCID = int(data[0])
	ss.ServerAddr = string(data[3 : 3+addrLen])
	ss.Port = int(binary.BigEndian.Uint16(data[3+addrLen : 3+addrLen+2]))
	copy(ss.AuthKeyValue[:], data[3+addrLen+2:need])
	ss.HasAuthKey = anyNonZero(ss.AuthKeyValue[:])
	return ss, nil
}

// String encodes the session in the current format. It returns "" when there
// is no auth key, exactly like the reference implementation's save().
//
// Layout: version char '1' followed by urlsafe base64 of
// dc_id:u8 | addr_len:u16be | addr | port:u16be | key:256.
func (ss *StringSession) String() string {
	if !ss.HasAuthKey {
		return ""
	}
	addr := []byte(ss.ServerAddr)
	payload := make([]byte, 0, 3+len(addr)+2+256)
	payload = append(payload, byte(ss.DCID))
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(addr)))
	payload = append(payload, l[:]...)
	payload = append(payload, addr...)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(ss.Port))
	payload = append(payload, p[:]...)
	payload = append(payload, ss.AuthKeyValue[:]...)
	return string(CurrentVersion) + base64.URLEncoding.EncodeToString(payload)
}

func anyNonZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return true
		}
	}
	return false
}
