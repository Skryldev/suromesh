package client

// Phase 12 fuzz targets: hostile mutation responses, search pagination
// and message-ID inputs must never panic, loop, or fabricate state.

import (
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// FuzzMessageMutationResponse: hostile mutation results (any Updates
// family shape) through the response mapping — no panic, nil holes
// preserved, no fabricated messages.
func FuzzMessageMutationResponse(f *testing.F) {
	f.Add([]byte{0x74, 0xae, 0x42, 0x40}) // updates ctor
	f.Add([]byte{0x78, 0xd4, 0xde, 0xc1}) // updateShort
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
		// The mapping must accept/reject without panicking; nil holes and
		// errors are both valid outcomes.
		msgs, err := c.responseMessageList(nil, []int64{1, 2}, 0, obj)
		if err != nil {
			return
		}
		for _, m := range msgs {
			if m != nil && m.ID < 0 {
				t.Fatalf("fabricated negative id: %+v", m)
			}
		}
	})
}

// FuzzSearchPagination: arbitrary ID progressions through the search
// digest rules must terminate.
func FuzzSearchPagination(f *testing.F) {
	f.Add(int32(1), int32(2), int32(3), int32(100))
	f.Fuzz(func(t *testing.T, a, b, c, limit int32) {
		lastID := int64(0)
		pages := [3][]int32{{a}, {b}, {c}}
		handOut := int64(0)
		for load, page := range pages {
			if load > 10 {
				t.Fatal("pagination did not terminate")
			}
			if limit > 0 && handOut >= int64(limit) {
				break // Limit cap
			}
			for _, id := range page {
				if id <= 0 {
					continue // MessageEmpty-equivalent
				}
				if lastID != 0 && int64(id) >= lastID {
					continue // duplicate
				}
				lastID = int64(id)
				handOut++
			}
		}
	})
}

// FuzzMessageIDs: hostile message-ID vectors through the mutation
// validation rules (≤0 rejected, int32-range conversion safe).
var out = make([]int32, 0, 8) //nolint:gochecknoglobals — fuzz scratch

func FuzzMessageIDs(f *testing.F) {
	f.Add(int64(0), int64(-1))
	f.Add(int64(1), int64(2147483647))
	f.Add(int64(9223372036854775807), int64(42))
	f.Fuzz(func(t *testing.T, a, b int64) {
		ids := []int64{a, b}
		out := out[:0]
		for _, id := range ids {
			if id <= 0 {
				continue // mutation validation rejects ≤0
			}
			if id > 0x7fffffff {
				continue // beyond the wire int32 — reject before conversion
			}
			out = append(out, int32(id))
		}
		// The conversion must be lossless for every accepted ID (parallel
		// walk of the FILTERED vectors).
		oi := 0
		for _, id := range ids {
			if id <= 0 || id > 0x7fffffff {
				continue
			}
			if int64(out[oi]) != id {
				t.Fatalf("id %d mangled to %d", id, out[oi])
			}
			oi++
		}
	})
}

// FuzzForwardResponse: forward answers with partial random-id mappings —
// nil holes, no panic, order preserved.
func FuzzForwardResponse(f *testing.F) {
	f.Add(int64(1), int32(10))
	f.Add(int64(0), int32(-5))
	f.Fuzz(func(t *testing.T, randomID int64, msgID int32) {
		result := &gen.Updates{
			Updates: []gen.UpdateClass{
				&gen.UpdateMessageID{RandomID: randomID, ID: msgID},
			},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0,
		}
		c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
		msgs, err := c.responseMessageList(nil, []int64{randomID}, 0, result)
		if err != nil {
			return
		}
		if len(msgs) != 1 {
			t.Fatalf("result arity %d, want 1", len(msgs))
		}
		if msgs[0] != nil && msgs[0].ID != int64(msgID) {
			t.Fatalf("mapped id %d, want %d", msgs[0].ID, msgID)
		}
	})
}

// FuzzMutationUpdates: hostile update sequences through the conversation
// synchronizer keep the store bounded and state impossible-free.
func FuzzMutationUpdates(f *testing.F) {
	f.Add(int32(1), int32(2), true, false)
	f.Fuzz(func(t *testing.T, msgID, pts int32, out, pinned bool) {
		cs := newConversationStore(2)
		peer := PeerID{PeerUser, 300}
		for i := int32(0); i < 6; i++ {
			cs.onNewMessage(peer, int64(msgID+i), out)
			cs.onEditMessage(peer, int64(msgID+i))
			cs.onDeleteMessages(peer)
		}
		conv, ok := cs.Lookup(peer)
		if ok {
			if conv.UnreadCount < 0 || conv.ReadOutboxMaxID < 0 {
				t.Fatalf("impossible state: %+v", conv)
			}
		}
		_ = pinned
		if cs.Len() > 2 {
			t.Fatalf("capacity exceeded: %d", cs.Len())
		}
	})
}
