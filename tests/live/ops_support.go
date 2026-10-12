package live

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"strconv"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// ops_support.go: helpers shared by the validation ops.

// fakeHostMarker is the Host value the fake-wiring suite uses; sourceOf
// maps it to FAKE so the two validation sources can never be confused.
const fakeHostMarker = "fake.suromesh.invalid"

// liveTestText builds a unique, self-describing test message. Message text
// NEVER enters evidence (only its length/hash may).
func liveTestText(tag string) string {
	return fmt.Sprintf("suromesh-live %s %d", tag, time.Now().UnixNano())
}

// peerIDOf converts any PeerRef to its PeerID where possible (Peers carry
// IDs; usernames resolve to IDs after resolution).
func peerIDOf(ref client.PeerRef) client.PeerID {
	if id, ok := ref.(client.PeerID); ok {
		return id
	}
	if p, ok := ref.(client.Peer); ok {
		return pPeerID(p)
	}
	return client.PeerID{}
}

// PeerRef returns the configured test peer as a reference.
func (c Config) PeerRef() client.PeerRef {
	switch c.PeerKind {
	case "chat":
		id, _ := strconv.ParseInt(c.Peer, 10, 64)
		return client.PeerID{Kind: client.PeerChat, ID: id}
	case "channel":
		id, _ := strconv.ParseInt(c.Peer, 10, 64)
		return client.PeerID{Kind: client.PeerChannel, ID: id}
	default:
		return client.UsernameRef(c.Peer)
	}
}

// liveMsg is the shape-safe projection of an incoming message used for
// exactly-once accounting (peer kind, numeric id, direction — no content).
type liveMsg struct {
	Peer client.PeerID
	ID   int64
	Out  bool
}

// liveExtractMessage pulls the liveMsg projection out of a raw update.
// Unknown update shapes are ignored (they are counted by the caller).
func liveExtractMessage(upd tl.Object) (liveMsg, bool) {
	switch u := upd.(type) {
	case *gen.UpdateNewMessage:
		return liveExtractMessageClass(u.Message)
	case *gen.Updates:
		for _, up := range u.Updates {
			if m, ok := liveExtractMessage(up); ok {
				return m, true
			}
		}
	}
	return liveMsg{}, false
}

func liveExtractMessageClass(mc gen.MessageClass) (liveMsg, bool) {
	switch m := mc.(type) {
	case *gen.Message:
		out := liveMsg{ID: int64(m.ID), Out: m.Out}
		switch p := m.PeerID.(type) {
		case *gen.PeerUser:
			out.Peer = client.PeerID{Kind: client.PeerUser, ID: p.UserID}
		case *gen.PeerChat:
			out.Peer = client.PeerID{Kind: client.PeerChat, ID: p.ChatID}
		case *gen.PeerChannel:
			out.Peer = client.PeerID{Kind: client.PeerChannel, ID: p.ChannelID}
		default:
			return out, false
		}
		return out, true
	case *gen.MessageService:
		out := liveMsg{ID: int64(m.ID), Out: m.Out}
		switch p := m.PeerID.(type) {
		case *gen.PeerUser:
			out.Peer = client.PeerID{Kind: client.PeerUser, ID: p.UserID}
		case *gen.PeerChat:
			out.Peer = client.PeerID{Kind: client.PeerChat, ID: p.ChatID}
		case *gen.PeerChannel:
			out.Peer = client.PeerID{Kind: client.PeerChannel, ID: p.ChannelID}
		default:
			return out, false
		}
		return out, true
	}
	return liveMsg{}, false
}

// pPeerID extracts the PeerID from a resolved domain Peer via its exported
// identity fields (User.ID/Chat.ID/Channel.ID + Kind).
func pPeerID(p client.Peer) client.PeerID {
	switch v := p.(type) {
	case *client.User:
		return client.PeerID{Kind: client.PeerUser, ID: v.ID}
	case *client.Chat:
		return client.PeerID{Kind: client.PeerChat, ID: v.ID}
	case *client.Channel:
		return client.PeerID{Kind: client.PeerChannel, ID: v.ID}
	}
	return client.PeerID{}
}

// patternReader streams the deterministic live-test pattern
// (byte(pos % 251)) without materializing the file: O(chunk) memory.
type patternReader struct {
	pos       int64
	remaining int64
}

func newPatternReader(size int64) io.Reader { return &patternReader{remaining: size} }

func (r *patternReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = byte((r.pos + i) % 251)
	}
	r.pos += n
	r.remaining -= n
	return int(n), nil
}

// patternSum computes the expected SHA-256 of size pattern bytes without
// materializing them.
func patternSum(size int64) string {
	h := sha256.New()
	var chunk [4096]byte
	left := size
	for left > 0 {
		n := int64(len(chunk))
		if left < n {
			n = left
		}
		for i := int64(0); i < n; i++ {
			chunk[i] = byte((size - left + i) % 251)
		}
		h.Write(chunk[:n])
		left -= n
	}
	return "sha256:" + fmt.Sprintf("%x", h.Sum(nil))
}

// hashSink is exported for tests that need a generic hashing io.Writer.

var _ hash.Hash = sha256.New()

// disconnectedClient builds a client bound to a fresh in-memory session
// that is deliberately NOT connected (for the not-ready error probe).
func disconnectedClient(s *Suite) (*client.Client, error) {
	hooks := s.Trace.Hooks()
	return client.New(client.Config{
		Session: session.New(),
		Host:    "127.0.0.1:1",
		Hooks:   hooks,
	})
}

// probePing is a minimal request used by the not-ready probe.
func newProbePing() tl.Request { return &gen.PingRequest{PingID: 1} }

var _ = binary.LittleEndian
var _ = crypto.NewAuthKey
var _ = tl.Object(nil)
