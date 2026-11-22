package upstream

// SDK conformance tests (Phase 26 Layer C/E): verify the adapter against
// the REAL SDK — session blob symmetry with a live SDK session object,
// error mapping exhaustiveness, update conversion, and message
// conversion. No network; the SDK client is constructed but never
// dialed (construction is I/O-free by SDK contract).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	sdkclient "github.com/Skryldev/suromesh/client"
	sdkcrypto "github.com/Skryldev/suromesh/crypto"
	sdkrpc "github.com/Skryldev/suromesh/rpc"
	sdksession "github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl/gen"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// realSession builds an SDK session with deterministic, NON-SECRET
// synthetic material (test-only bytes; never real key material).
func realSession(t *testing.T, authorized bool) *sdksession.Session {
	t.Helper()
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i * 13 % 251)
	}
	s := sdksession.NewWithKey(sdkcrypto.NewAuthKey(kb))
	if err := s.SetDC(2, "im-server.splus.ir", 443); err != nil {
		t.Fatal(err)
	}
	if authorized {
		if err := s.SetAuthorized(); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetUpdateState(sdksession.UpdateStateRecord{Pts: 100, Qts: 50, Date: 1700000000, Seq: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChannelUpdateState(300, 42); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChannelUpdateState(301, 43); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSessionBlob_ExportImportSymmetry: the service-owned blob must
// round-trip every meaningful field of a REAL SDK session.
func TestSessionBlob_ExportImportSymmetry(t *testing.T) {
	for _, authorized := range []bool{true, false} {
		original := realSession(t, authorized)
		blob, err := marshalSession(original)
		if err != nil {
			t.Fatalf("marshal (authorized=%v): %v", authorized, err)
		}
		// The blob must NOT contain the raw key material in plaintext.
		if bytes.Contains(blob, original.AuthKey().Key[:8]) {
			t.Fatal("raw key bytes leaked into the JSON blob (base64 required)")
		}
		restored, err := unmarshalSession(blob)
		if err != nil {
			t.Fatalf("unmarshal (authorized=%v): %v", authorized, err)
		}

		// Field-by-field semantic equality against the ORIGINAL session.
		k1, k2 := original.AuthKey(), restored.AuthKey()
		if (k1 == nil || !k1.HasValue) != (k2 == nil || !k2.HasValue) {
			t.Fatalf("authorized=%v: key presence mismatch", authorized)
		}
		if k1 != nil && k2 != nil && k1.HasValue && k2.HasValue {
			if k1.KeyID != k2.KeyID {
				t.Fatalf("authorized=%v: key id mismatch", authorized)
			}
			if k1.Key != k2.Key {
				t.Fatalf("authorized=%v: key bytes mismatch", authorized)
			}
		}
		dc1, addr1, port1 := original.DC()
		dc2, addr2, port2 := restored.DC()
		if dc1 != dc2 || addr1 != addr2 || port1 != port2 {
			t.Fatalf("dc mismatch: (%d,%s,%d) vs (%d,%s,%d)", dc1, addr1, port1, dc2, addr2, port2)
		}
		st1, ok1 := original.UpdateState()
		st2, ok2 := restored.UpdateState()
		if ok1 != ok2 || st1 != st2 {
			t.Fatalf("update frontier mismatch: %+v vs %+v", st1, st2)
		}
		c1, c2 := original.ChannelUpdateStates(), restored.ChannelUpdateStates()
		if len(c1) != len(c2) {
			t.Fatalf("channel states count mismatch: %d vs %d", len(c1), len(c2))
		}
		// The SDK stores channel states in a map (random iteration order):
		// compare order-independently; the BLOB is sorted (deterministic).
		byID := map[int64]sdksession.ChannelUpdateRecord{}
		for _, c := range c2 {
			byID[c.ChannelID] = c
		}
		for _, c := range c1 {
			r, ok := byID[c.ChannelID]
			if !ok || r.Pts != c.Pts {
				t.Fatalf("channel state %d mismatch: want pts %d, got %+v",
					c.ChannelID, c.Pts, r)
			}
		}
		if (original.AuthState() == sdksession.StateAuthorized) !=
			(restored.AuthState() == sdksession.StateAuthorized) {
			t.Fatalf("authorized flag mismatch (authorized=%v)", authorized)
		}
		// Double round trip: stability (no drift).
		blob2, err := marshalSession(restored)
		if err != nil {
			t.Fatal(err)
		}
		var v1map, v2map map[string]any
		if err := json.Unmarshal(blob, &v1map); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(blob2, &v2map); err != nil {
			t.Fatal(err)
		}
		if len(v1map) != len(v2map) {
			t.Fatal("re-marshalled blob shape drifted")
		}
	}
}

// TestSessionBlob_InvalidInputs: corrupted/versioned blobs fail typed.
func TestSessionBlob_InvalidInputs(t *testing.T) {
	cases := []struct {
		name string
		blob []byte
	}{
		{"not json", []byte("this is not json")},
		{"wrong version", []byte(`{"version":99}`)},
		{"bad base64 key", []byte(`{"version":1,"has_auth_key":true,"auth_key_b64":"!!!"}`)},
		{"short key", []byte(`{"version":1,"has_auth_key":true,"auth_key_b64":"aaaa"}`)},
		{"empty", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := unmarshalSession(tc.blob)
			if !errors.Is(err, domain.ErrCrypto) {
				t.Fatalf("err = %v, want ErrCrypto", err)
			}
		})
	}
}

// TestSessionBlob_RebindAfterRevocation: after LogOut the SDK clears the
// authorized flag; the exported blob must reflect UNAUTHORIZED state (no
// stale authorized flag resurrecting a revoked session).
func TestSessionBlob_RevocationReflected(t *testing.T) {
	s := realSession(t, true)
	s.SetUnauthorized()
	blob, err := marshalSession(s)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := unmarshalSession(blob)
	if err != nil {
		t.Fatal(err)
	}
	if restored.AuthState() == sdksession.StateAuthorized {
		t.Fatal("revoked session restored as authorized")
	}
	// The key itself persists (transport-level security is separate from
	// authorization — matches the SDK's own semantics).
	if k := restored.AuthKey(); k == nil || !k.HasValue {
		t.Fatal("key must survive revocation (matches SDK semantics)")
	}
}

// ---- error mapping table (Layer C) ----------------------------------------

// TestTranslate_ErrorMappingTable: exhaustive mapping of SDK error
// shapes to the domain taxonomy.
func TestTranslate_ErrorMappingTable(t *testing.T) {
	flood := &sdkrpc.RPCError{Code: 420, Type: "FLOOD_WAIT", Argument: 9}
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"context canceled", context.Canceled, context.Canceled},
		{"context deadline", context.DeadlineExceeded, context.DeadlineExceeded},
		{"wrapped cancel", fmtWrap(context.Canceled), context.Canceled},
		{"flood wait", flood, domain.ErrFloodWait},
		{"flood wait seconds", flood, nil}, // seconds checked separately
		{"auth key unregistered", &sdkrpc.RPCError{Code: 401, Type: "AUTH_KEY_UNREGISTERED"}, domain.ErrSessionRevoked},
		{"session revoked", &sdkrpc.RPCError{Code: 401, Type: "SESSION_REVOKED"}, domain.ErrSessionRevoked},
		{"session expired", &sdkrpc.RPCError{Code: 401, Type: "SESSION_EXPIRED"}, domain.ErrSessionRevoked},
		{"file ref expired", &sdkrpc.RPCError{Code: 400, Type: "FILE_REFERENCE_EXPIRED"}, domain.ErrFileReferenceExpired},
		{"peer id invalid", &sdkrpc.RPCError{Code: 400, Type: "PEER_ID_INVALID"}, domain.ErrInvalidInput},
		{"peer hash invalid", &sdkrpc.RPCError{Code: 400, Type: "PEER_HASH_INVALID"}, domain.ErrInvalidInput},
		{"channel private", &sdkrpc.RPCError{Code: 400, Type: "CHANNEL_PRIVATE"}, domain.ErrNotFound},
		{"message id invalid", &sdkrpc.RPCError{Code: 400, Type: "MESSAGE_ID_INVALID"}, domain.ErrNotFound},
		{"generic rpc error", &sdkrpc.RPCError{Code: 500, Type: "INTERNAL"}, domain.ErrUpstreamProtocol},
		{"connection lost wrapper", &sdkclient.ConnectionLostError{Err: errors.New("x")}, domain.ErrAmbiguousMutation},
		{"client closed", sdkclient.ErrClosed, domain.ErrAccountStopped},
		{"client not ready", sdkclient.ErrNotReady, domain.ErrAccountNotConnected},
		{"file ref expired sentinel", sdkclient.ErrFileReferenceExpired, domain.ErrFileReferenceExpired},
	}
	for _, tc := range tests {
		if tc.want == nil {
			continue // handled below (seconds check)
		}
		t.Run(tc.name, func(t *testing.T) {
			got := translate(tc.err)
			if !errors.Is(got, tc.want) {
				t.Fatalf("translate(%v) = %v, want wrapping %v", tc.err, got, tc.want)
			}
		})
	}
	// Flood wait seconds preserved through the mapping.
	var fw *domain.FloodWaitError
	got := translate(flood)
	if !errors.As(got, &fw) || fw.Seconds != 9 {
		t.Fatalf("flood seconds: got %+v (%v)", fw, got)
	}
}

// TestTranslate_TransportAndNetwork: transport classes and net errors.
func TestTranslate_TransportAndNetwork(t *testing.T) {
	// Connection-refused shaped text → upstream unavailable.
	got := translate(errors.New("dial tcp: connection refused"))
	if !errors.Is(got, domain.ErrUpstreamUnavailable) {
		t.Fatalf("conn refused → %v", got)
	}
	// Unknown shapes → upstream protocol (internal family), never nil/success.
	got = translate(errors.New("something exotic"))
	if got == nil || errors.Is(got, context.Canceled) {
		t.Fatalf("exotic error must map to a classified failure, got %v", got)
	}
}

// ---- update conversion (Layer C/E) -----------------------------------------

// TestConvertUpdate_Families: every supported update family converts to
// the correct domain event with correct field mapping.
func TestConvertUpdate_Families(t *testing.T) {
	peer := &gen.PeerUser{UserID: 42}
	msg := &gen.Message{ID: 7, Message: "hello", PeerID: peer, Date: 1700000000, Out: true}

	ev, ok := convertUpdate(&gen.UpdateNewMessage{Message: msg})
	if !ok || ev.Kind != domain.UpdateNewMessage || ev.NewMessage == nil {
		t.Fatalf("UpdateNewMessage → %+v ok=%v", ev, ok)
	}
	if ev.NewMessage.ID != 7 || ev.NewMessage.Text != "hello" ||
		ev.NewMessage.PeerID.ID != 42 || ev.NewMessage.PeerID.Kind != domain.PeerKindUser ||
		ev.NewMessage.Direction != domain.DirectionOutgoing {
		t.Fatalf("message mapping: %+v", ev.NewMessage)
	}

	ev, ok = convertUpdate(&gen.UpdateEditMessage{Message: msg})
	if !ok || ev.Kind != domain.UpdateEditedMessage || ev.Edited == nil {
		t.Fatalf("UpdateEditMessage → %+v ok=%v", ev, ok)
	}

	ev, ok = convertUpdate(&gen.UpdateDeleteMessages{Messages: []int32{5, 6}})
	if !ok || ev.Kind != domain.UpdateDeletedMessages || ev.Deleted == nil ||
		len(ev.Deleted.MessageIDs) != 2 || ev.Deleted.MessageIDs[0] != 5 {
		t.Fatalf("UpdateDeleteMessages → %+v ok=%v", ev, ok)
	}

	inbox := &gen.UpdateReadHistoryInbox{Peer: peer, MaxID: 99}
	ev, ok = convertUpdate(inbox)
	if !ok || ev.Kind != domain.UpdateReadState || ev.Read == nil ||
		ev.Read.MaxID != 99 || ev.Read.Outgoing {
		t.Fatalf("ReadInbox → %+v ok=%v", ev, ok)
	}
	outbox := &gen.UpdateReadHistoryOutbox{Peer: peer, MaxID: 100}
	ev, ok = convertUpdate(outbox)
	if !ok || ev.Read == nil || !ev.Read.Outgoing {
		t.Fatalf("ReadOutbox → %+v ok=%v", ev, ok)
	}

	// Batched Updates container: first convertible member wins.
	batch := &gen.Updates{Updates: []gen.UpdateClass{
		&gen.UpdateReadHistoryInbox{Peer: peer, MaxID: 1},
		&gen.UpdateNewMessage{Message: msg},
	}}
	ev, ok = convertUpdate(batch)
	if !ok {
		t.Fatal("batched update did not convert")
	}

	// Unsupported families (state-only) are dropped from delivery.
	if _, ok := convertUpdate(&gen.UpdateConfig{}); ok {
		t.Fatal("unsupported family must not convert")
	}
}

// ---- message conversion (Layer C/E) ----------------------------------------

// TestConvertMessage_Fidelity: SDK message → domain message preserves
// ids, text, direction, reply, entities, media, service info.
func TestConvertMessage_Fidelity(t *testing.T) {
	reply := int64(41)
	cl, clerr := sdkclient.New(sdkclient.Config{Session: sdksession.New()})
	if clerr != nil {
		t.Fatal(clerr)
	}
	c := &clientSession{c: cl}
	msg := &sdkclient.Message{
		ID:     42,
		PeerID: sdkclient.PeerID{Kind: sdkclient.PeerChat, ID: 55},
		Text:   "پیام آزمایشی",
		Date:   time.Unix(1700000000, 0),
		Out:    true,
		Reply:  &sdkclient.ReplyInfo{MessageID: reply},
		TextEntities: []sdkclient.TextEntity{
			{Kind: sdkclient.TextEntityBold, Offset: 0, Length: 5},
			{Kind: sdkclient.TextEntityURL, Offset: 6, Length: 10, URL: "https://x.example"},
		},
		Media: &sdkclient.Document{ID: 9, Size: 1000, MimeType: "application/pdf"},
	}
	got := convertMessage(msg)
	if got.ID != 42 || got.PeerID.ID != 55 || got.PeerID.Kind != domain.PeerKindChat ||
		got.Text != "پیام آزمایشی" || got.Direction != domain.DirectionOutgoing ||
		got.ReplyTo == nil || *got.ReplyTo != 41 {
		t.Fatalf("core fields: %+v", got)
	}
	if len(got.Entities) != 2 || got.Entities[0].Kind != domain.EntityBold ||
		got.Entities[1].Kind != domain.EntityURL || got.Entities[1].URL != "https://x.example" {
		t.Fatalf("entities: %+v", got.Entities)
	}
	if got.Media == nil || got.Media.Kind != domain.MediaKindDocument ||
		got.Media.SizeBytes != 1000 {
		t.Fatalf("media: %+v", got.Media)
	}
	_ = c // (clientSession retained for future conversion tests needing the client)
}

// TestPeerConversion_RoundTrip: domain ↔ SDK peer ids.
func TestPeerConversion_RoundTrip(t *testing.T) {
	for _, kind := range []sdkclient.PeerKind{sdkclient.PeerUser, sdkclient.PeerChat, sdkclient.PeerChannel} {
		sdkID := sdkclient.PeerID{Kind: kind, ID: 12345}
		dom := convertPeerID(sdkID)
		back := sdkPeerID(dom)
		if back != sdkID {
			t.Fatalf("round trip %v → %v → %v", sdkID, dom, back)
		}
	}
}

// helpers -------------------------------------------------------------------

type wrapErr struct{ inner error }

func (w wrapErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w wrapErr) Unwrap() error { return w.inner }

func fmtWrap(err error) error { return wrapErr{inner: err} }
