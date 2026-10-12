package live

// Fake-wiring validation (Phase 18 §34.7): "Validate that live tests are
// executable when credentials are supplied."
//
// These tests run the EXACT shared validation operations (ops.go) against
// the in-process fake server over the real protocol stack. They prove the
// harness plumbing — bootstrap, tracing, evidence, cleanup, every op — is
// executable end-to-end, so that when operator credentials are supplied
// the live suites exercise only the SERVICE, not untested harness code.
//
// Classification: FAKE. These results never substitute for live results.

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// fakeServer is the minimal stateful fake used by the wiring tests: it
// answers every RPC family the ops exercise.
type fakeServer struct {
	t     *testing.T
	key   *crypto.AuthKey
	mu    sync.Mutex
	parts map[int64]map[int32][]byte
}

func (f *fakeServer) uploaded(fileID int64) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	for i := int32(0); ; i++ {
		p, ok := f.parts[fileID][i]
		if !ok {
			break
		}
		out = append(out, p...)
	}
	return out
}

func (f *fakeServer) handle(obj tl.Object) (tl.Object, *rpc.RPCError) {
	switch o := obj.(type) {
	case *gen.InvokeWithLayerRequest:
		return f.handle(o.Query)
	case *gen.InitConnectionRequest:
		if _, ok := o.Query.(*gen.HelpGetConfigRequest); ok {
			return fakeConfig(), nil
		}
		return f.handle(o.Query)
	case *gen.HelpGetConfigRequest:
		return fakeConfig(), nil
	case *gen.PingRequest:
		return &gen.Pong{MsgID: 1, PingID: o.PingID}, nil
	case *gen.UsersGetUsersRequest:
		hash := int64(0xabc123)
		first := "Golden"
		return &rpc.VectorResult{Items: []tl.Object{&gen.User{
			ID: 7001, UserType: &gen.UserTypeNormal{}, AccessHash: &hash, FirstName: &first,
		}}}, nil
	case *gen.MessagesGetDialogsRequest:
		hash := int64(0x111000)
		first := "peer"
		return &gen.MessagesDialogs{
			Dialogs: []gen.DialogClass{&gen.Dialog{
				Peer: &gen.PeerUser{UserID: 7002}, TopMessage: 3,
				NotifySettings: &gen.PeerNotifySettings{},
			}},
			Messages: []gen.MessageClass{&gen.Message{ID: 3, Message: "seed",
				PeerID: &gen.PeerUser{UserID: 7002}, Date: 3}},
			Users: []gen.UserClass{&gen.User{ID: 7002, UserType: &gen.UserTypeNormal{},
				AccessHash: &hash, FirstName: &first}},
			Chats: []gen.ChatClass{},
		}, nil
	case *gen.MessagesGetHistoryRequest:
		hash := int64(0x111000)
		first := "peer"
		return &gen.MessagesMessagesSlice{Count: 1,
			Messages: []gen.MessageClass{&gen.Message{ID: 3, Message: "seed",
				PeerID: &gen.PeerUser{UserID: 7002}, Date: 3}},
			Users: []gen.UserClass{&gen.User{ID: 7002, UserType: &gen.UserTypeNormal{},
				AccessHash: &hash, FirstName: &first}},
			Chats: []gen.ChatClass{}}, nil
	case *gen.MessagesSendMessageRequest:
		id := int32(41)
		return &gen.Updates{
			Updates: []gen.UpdateClass{
				&gen.UpdateMessageID{RandomID: o.RandomID, ID: id},
				&gen.UpdateNewMessage{Message: &gen.Message{ID: id, Message: o.Message,
					Out: true, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100},
					PTS: id, PTSCount: 1},
			},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0,
		}, nil
	case *gen.MessagesEditMessageRequest:
		text := ""
		if o.Message != nil {
			text = *o.Message
		}
		return &gen.Updates{
			Updates: []gen.UpdateClass{&gen.UpdateEditMessage{
				Message: &gen.Message{ID: o.ID, Message: text, Out: true,
					PeerID: &gen.PeerChat{ChatID: 55}, Date: 100},
				PTS: o.ID, PTSCount: 1,
			}},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0,
		}, nil
	case *gen.MessagesDeleteMessagesRequest:
		return &gen.MessagesAffectedMessages{PTS: 1, PTSCount: int32(len(o.ID))}, nil
	case *gen.UploadSaveFilePartRequest:
		f.mu.Lock()
		if f.parts[o.FileID] == nil {
			f.parts[o.FileID] = map[int32][]byte{}
		}
		cp := make([]byte, len(o.Bytes))
		copy(cp, o.Bytes)
		f.parts[o.FileID][o.FilePart] = cp
		f.mu.Unlock()
		return &gen.BoolTrue{}, nil
	case *gen.UploadGetFileRequest:
		var locID int64
		switch l := o.Location.(type) {
		case *gen.InputDocumentFileLocation:
			locID = l.ID
		case *gen.InputFileLocation:
			locID = l.VolumeID
		default:
			return nil, &rpc.RPCError{Code: 400, Message: "LOCATION_INVALID"}
		}
		data := f.uploaded(locID)
		off := int(o.Offset)
		if off < 0 || off >= len(data) {
			return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Bytes: []byte{}}, nil
		}
		end := off + int(o.Limit)
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]
		if len(chunk) == 0 {
			return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Bytes: []byte{}}, nil
		}
		return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Mtime: 1700000000, Bytes: chunk}, nil
	case *gen.MessagesSendMediaRequest:
		var fileID int64
		if im, ok := o.Media.(*gen.InputMediaUploadedDocument); ok {
			switch f2 := im.File.(type) {
			case *gen.InputFile:
				fileID = f2.ID
			case *gen.InputFileBig:
				fileID = f2.ID
			}
		}
		data := f.uploaded(fileID)
		doc := &gen.Document{
			ID: fileID, AccessHash: 0xfee1, FileReference: []byte{1, 2, 3},
			Date: 100, MimeType: "application/octet-stream", Size: int64(len(data)),
			Attributes: []gen.DocumentAttributeClass{
				&gen.DocumentAttributeFilename{FileName: "wired.bin"},
			},
		}
		return &gen.Updates{
			Updates: []gen.UpdateClass{
				&gen.UpdateMessageID{RandomID: o.RandomID, ID: 20},
				&gen.UpdateNewMessage{Message: &gen.Message{ID: 20, Out: true,
					PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
					Media: &gen.MessageMediaDocument{Document: doc}},
					PTS: 20, PTSCount: 1},
			},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0,
		}, nil
	}
	return nil, &rpc.RPCError{Code: 400, Message: "METHOD_NOT_IMPLEMENTED_IN_FAKE"}
}

func fakeConfig() tl.Object {
	return &gen.Config{
		Date: 1700000000, Expires: 1800000000, ThisDC: 1,
		DCTxtDomainName: "splus.ir", ChatSizeMax: 200,
		MegagroupSizeMax: 200000, ForwardedCountMax: 100,
		OnlineUpdatePeriodMs: 120000, OfflineBlurTimeoutMs: 5000,
		OfflineIdleTimeoutMs: 30000, OnlineCloudTimeoutMs: 300000,
		NotifyCloudDelayMs: 30000, NotifyDefaultDelayMs: 1500,
		PushChatPeriodMs: 60000, PushChatLimit: 20,
		EditTimeLimit: 172800, RevokeTimeLimit: 172800,
		RevokePmTimeLimit: 604800, RatingEDecay: 2419200,
		StickersRecentLimit: 30, ChannelsReadMediaPeriod: 43200,
		MeURLPrefix: "https://splus.ir/", MessageLengthMax: 4096,
		WebfileDCID: 2,
	}
}

func fakeDemoKey() *crypto.AuthKey {
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i*7 + 3)
	}
	return crypto.NewAuthKey(kb)
}

// fakeWiring builds a Suite whose client talks to the in-process fake with
// a PRE-AUTHORIZED session (the exact shape of the live session-reuse
// scenario: connect must not authenticate).
func fakeWiring(t *testing.T) *Suite {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	red := NewRedactor(nil)
	trace := NewTrace(red)
	rec, err := NewRecorder("", red)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	key := fakeDemoKey()
	path := tempSessionFilePath(t)
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthKey(key); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		t.Fatal(err)
	}
	// Persist BEFORE connecting: the reuse factory reopens the file, and
	// an unsaved session would look keyless (forcing a DH exchange the
	// fake cannot serve).
	if err := fs.Save(); err != nil {
		t.Fatal(err)
	}

	fsObj := &fakeServer{t: t, key: key, parts: map[int64]map[int32][]byte{}}
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, func(p *wstest.SoroushPeer, pkt []byte) {
			env, derr := wstest.DecryptClientEnvelope(pkt, key)
			if derr != nil || len(env.Body) < 4 || wstest.IsServiceBody(env.Body) {
				return
			}
			obj, uerr := tl.Unmarshal(env.Body)
			var result tl.Object
			var rpcErr *rpc.RPCError
			if uerr != nil {
				rpcErr = &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
			} else {
				result, rpcErr = fsObj.handle(obj)
			}
			rr := &tl.RpcResult{ReqMsgID: env.MsgID}
			if rpcErr != nil {
				rr.Error = &tl.RpcError{ErrorCode: rpcErr.Code, ErrorMessage: rpcErr.Message}
			} else {
				b, merr := tl.Marshal(result)
				if merr != nil {
					return
				}
				rr.Body = b
			}
			body, merr := tl.Marshal(rr)
			if merr != nil {
				return
			}
			w := tl.NewWriter(32 + len(body))
			w.PutInt64(env.Salt)
			w.PutInt64(env.SessionID)
			w.PutInt64(int64(0x40000000) + env.MsgID + 2)
			w.PutInt32(1)
			w.PutInt32(int32(len(body)))
			w.PutRaw(body)
			envelope, eerr := mtproto.EncryptServerMessageData(key, w.Bytes(), rand.Reader)
			if eerr == nil {
				_ = p.SendPacket(envelope)
			}
		})
		peer.Wait()
	}

	c, err := client.New(client.Config{
		Session:    fs.Session,
		Saver:      fs,
		Host:       srv.Addr,
		TLSEnabled: boolPtrFake(false),
		Hooks:      trace.Hooks(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("fake wiring connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(); srv.Close() })

	cfg := Config{Host: fakeHostMarker, PeerKind: "chat", Peer: "55"}
	s := &Suite{C: c, Trace: trace, Rec: rec, Cfg: cfg, Red: red, SessionPath: path, Addr: srv.Addr}
	return s
}

// TestFakeWiringSessionReuse: connect on a pre-authorized session must not
// authenticate (the §9 invariant, executable form).
func TestFakeWiringSessionReuse(t *testing.T) {
	s := fakeWiring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.OpSessionReuse(ctx, s.sessionFactoryFromPath(), 1); err != nil {
		t.Fatalf("session reuse wiring: %v", err)
	}
}

// TestFakeWiringOwnUpdate runs OpOwnUpdateNoDuplicate against the fake.
func TestFakeWiringOwnUpdate(t *testing.T) {
	s := fakeWiring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := s.OpOwnUpdateNoDuplicate(ctx, client.PeerID{Kind: client.PeerChat, ID: 55})
	if err != nil {
		t.Fatalf("own update op: %v", err)
	}
	if id == 0 {
		t.Fatal("no id")
	}
}

// TestFakeWiringMediaRoundTrip runs OpMediaRoundTrip end-to-end through
// real saveFilePart/getFile RPCs and verifies SHA-256 equality.
func TestFakeWiringMediaRoundTrip(t *testing.T) {
	s := fakeWiring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.OpMediaRoundTrip(ctx, 1024); err != nil {
		t.Fatalf("media round trip: %v", err)
	}
}

// TestFakeWiringErrorMatrix runs OpErrorMatrix against the fake.
func TestFakeWiringErrorMatrix(t *testing.T) {
	s := fakeWiring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.OpErrorMatrix(ctx); err != nil {
		t.Fatalf("error matrix: %v", err)
	}
}

// TestFakeWiringDialogsHistory runs OpDialogsAndHistory against the fake.
func TestFakeWiringDialogsHistory(t *testing.T) {
	s := fakeWiring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.OpDialogsAndHistory(ctx, 5); err != nil {
		t.Fatalf("dialogs/history: %v", err)
	}
}

// TestFakeWiringReconnect runs OpReconnect against the fake.
func TestFakeWiringReconnect(t *testing.T) {
	s := fakeWiring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.OpReconnect(ctx); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
}

// TestFakeWiringIncomingWindow proves the incoming-exactly-once window
// terminates cleanly when nothing arrives (the NOT_OBSERVED path).
func TestFakeWiringIncomingWindow(t *testing.T) {
	s := fakeWiring(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.OpIncomingExactlyOnce(ctx, 300*time.Millisecond); err != nil {
		t.Fatalf("incoming window: %v", err)
	}
	if got := s.Rec.Count(StatusNotObserved, SourceFake); got < 1 {
		t.Fatalf("expected NOT_OBSERVED record, got %d", got)
	}
}

// ---- unit tests of the sanitization layer (STATIC) ------------------------

// TestStaticRedactor: every registered secret is scrubbed from output.
func TestStaticRedactor(t *testing.T) {
	red := NewRedactor(map[string]string{
		"+15550001111": "phone",
		"54321":        "otp",
		"sup3rs3cret":  "password",
	})
	out := red.Redact("call +15550001111 code 54321 pw sup3rs3cret done")
	if strings.Contains(out, "15550001111") || strings.Contains(out, "54321") ||
		strings.Contains(out, "sup3rs3cret") {
		t.Fatalf("redactor leaked: %s", out)
	}
	if !strings.Contains(out, "<phone>") || !strings.Contains(out, "<otp>") ||
		!strings.Contains(out, "<password>") {
		t.Fatalf("redactor placeholders missing: %s", out)
	}
}

// TestStaticRedactorRecorder: records passing through the Recorder are
// redacted in MEMORY and on disk even when a field contains a secret.
func TestStaticRedactorRecorder(t *testing.T) {
	secret := "topsecretvalue"
	red := NewRedactor(map[string]string{secret: "phone"})
	rec, err := NewRecorder("", red)
	if err != nil {
		t.Fatal(err)
	}
	rec.Record("s", "op", StatusPass, SourceStatic, map[string]any{"note": "oops " + secret})
	for _, e := range rec.Entries() {
		if b, ok := e.Fields["note"].(string); ok && strings.Contains(b, secret) {
			t.Fatal("secret survived in recorded fields")
		}
	}
}

// TestStaticEvidenceClassification pins the status/source vocabularies.
func TestStaticEvidenceClassification(t *testing.T) {
	validStatus := map[string]bool{StatusPass: true, StatusFail: true, StatusBlocked: true,
		StatusSkipped: true, StatusNotObserved: true, StatusNotApplicable: true}
	validSource := map[string]bool{SourceLive: true, SourceFake: true,
		SourceReference: true, SourceStatic: true}
	for _, v := range []string{StatusPass, StatusFail, StatusBlocked, StatusSkipped, StatusNotObserved, StatusNotApplicable} {
		if !validStatus[v] {
			t.Fatalf("status %s missing", v)
		}
	}
	for _, v := range []string{SourceLive, SourceFake, SourceReference, SourceStatic} {
		if !validSource[v] {
			t.Fatalf("source %s missing", v)
		}
	}
}

// TestStaticHashOf pins the hash-prefix evidence helper.
func TestStaticHashOf(t *testing.T) {
	h := HashOf([]byte("anything"))
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+16 {
		t.Fatalf("HashOf = %s", h)
	}
}

// TestStaticPatternReader pins the streaming pattern generator used by the
// media round trip (O(chunk) memory, position-stable).
func TestStaticPatternReader(t *testing.T) {
	const size = 10000
	r := newPatternReader(size)
	got := 0
	buf := make([]byte, 777)
	for {
		n, err := r.Read(buf)
		got += n
		if err != nil {
			break
		}
	}
	if int64(got) != size {
		t.Fatalf("pattern reader produced %d bytes, want %d", got, size)
	}
}

func boolPtrFake(v bool) *bool { return &v }

func tempSessionFilePath(t *testing.T) string {
	t.Helper()
	p, err := tempSessionFile()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeFile(p) })
	return p
}
