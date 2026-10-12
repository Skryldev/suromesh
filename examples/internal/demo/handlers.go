package demo

import (
	"sync"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// This file wires the demo fake's RPC handlers. Every handler mirrors the
// response shape the real server family produces; only the demo-specific
// data (fixed user ids, in-memory byte stores) is synthetic.

const (
	demoSelfID    = int64(7001)
	demoSelfHash  = int64(0xabc123)
	demoPeerID    = int64(55)
	demoFileID    = int64(930)
	demoFileHash  = int64(0xfee1)
	demoChunkSize = 128 * 1024
)

func selfUser() *gen.User {
	id, hash := demoSelfID, demoSelfHash
	first := "Golden"
	return &gen.User{ID: id, UserType: &gen.UserTypeNormal{}, AccessHash: &hash, FirstName: &first}
}

// InstallAuth answers auth.sendCode / auth.signIn (fixed code "12345") and
// users.getUsers. The example then drives the flow through the public API.
func InstallAuth(s *Server, code string) {
	prev := s.responder
	s.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
		}
		switch o := obj.(type) {
		case *gen.AuthSendCodeRequest:
			return &gen.AuthSentCode{
				Type_:         &gen.AuthSentCodeTypeSms{Length: 5},
				PhoneCodeHash: "demo-hash",
				Timeout:       int32p(90),
			}, nil
		case *gen.AuthSignInRequest:
			if o.PhoneCode == nil || *o.PhoneCode != code {
				return nil, &rpc.RPCError{Code: 400, Message: "PHONE_CODE_INVALID"}
			}
			return &gen.AuthAuthorization{User: selfUser()}, nil
		}
		if ctor == 0x0d91a548 { // users.getUsers
			return &rpc.VectorResult{Items: []tl.Object{selfUser()}}, nil
		}
		return prev(ctor, body)
	})
}

// InstallMessaging answers dialogs/history/send/edit/delete with an echo
// peer ("chat 55") and pushes the sent message back as an update, matching
// the own-update semantics of the real server.
func InstallMessaging(s *Server) {
	prev := s.responder
	var mu sync.Mutex
	nextID := int64(10)
	// Seed one history message so the example's history read is non-empty.
	store := map[int64]*gen.Message{3: {
		ID: 3, Message: "seeded history message", Out: false,
		PeerID: &gen.PeerChat{ChatID: demoPeerID}, Date: 3,
	}}
	peerHash := int64(0xd1a709)
	dialogPeer := &gen.User{ID: 7002, UserType: &gen.UserTypeNormal{}, AccessHash: &peerHash}
	dialogUsers := func() []gen.UserClass { return []gen.UserClass{selfUser(), dialogPeer} }

	pushed := func(m *gen.Message) {
		_ = s.PushUpdate(&gen.Updates{
			Updates: []gen.UpdateClass{
				&gen.UpdateMessageID{RandomID: 0, ID: int32(m.ID)},
				&gen.UpdateNewMessage{Message: m, PTS: int32(m.ID), PTSCount: 1},
			},
			Users: []gen.UserClass{selfUser()}, Chats: []gen.ChatClass{},
			Date: 100, Seq: 0,
		})
	}

	s.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
		}
		switch o := obj.(type) {
		case *gen.MessagesGetDialogsRequest:
			m := &gen.Message{ID: 3, Message: "hello from the fake",
				PeerID: &gen.PeerUser{UserID: 7002}, Date: 3}
			return &gen.MessagesDialogs{
				Dialogs: []gen.DialogClass{&gen.Dialog{
					Peer: &gen.PeerUser{UserID: 7002}, TopMessage: 3,
					NotifySettings: &gen.PeerNotifySettings{},
				}},
				Messages: []gen.MessageClass{m},
				Users:    dialogUsers(), Chats: []gen.ChatClass{},
			}, nil

		case *gen.MessagesGetHistoryRequest:
			mu.Lock()
			msgs := make([]gen.MessageClass, 0, len(store))
			for _, m := range store {
				msgs = append(msgs, m)
			}
			mu.Unlock()
			return &gen.MessagesMessagesSlice{Count: int32(len(msgs)),
				Messages: msgs, Users: dialogUsers(),
				Chats: []gen.ChatClass{}}, nil

		case *gen.MessagesSendMessageRequest:
			mu.Lock()
			id := nextID
			nextID++
			m := &gen.Message{ID: int32(id), Message: o.Message, Out: true,
				PeerID: peerWire(o.Peer), Date: 100}
			store[id] = m
			mu.Unlock()
			pushed(m)
			return &gen.Updates{
				Updates: []gen.UpdateClass{
					&gen.UpdateMessageID{RandomID: o.RandomID, ID: int32(id)},
					&gen.UpdateNewMessage{Message: m, PTS: int32(id), PTSCount: 1},
				},
				Users: []gen.UserClass{selfUser()}, Chats: []gen.ChatClass{},
				Date: 100, Seq: 0,
			}, nil

		case *gen.MessagesEditMessageRequest:
			text := ""
			if o.Message != nil {
				text = *o.Message
			}
			mu.Lock()
			m := &gen.Message{ID: o.ID, Message: text, Out: true,
				PeerID: &gen.PeerChat{ChatID: demoPeerID}, Date: 100}
			store[int64(o.ID)] = m
			mu.Unlock()
			return &gen.Updates{
				Updates: []gen.UpdateClass{&gen.UpdateEditMessage{
					Message: m, PTS: o.ID, PTSCount: 1,
				}},
				Users: []gen.UserClass{selfUser()}, Chats: []gen.ChatClass{},
				Date: 100, Seq: 0,
			}, nil

		case *gen.MessagesDeleteMessagesRequest:
			return &gen.MessagesAffectedMessages{PTS: 1, PTSCount: int32(len(o.ID))}, nil
		}
		return prev(ctor, body)
	})
}

// InstallMedia stores uploaded parts in memory and serves upload.getFile
// from them; messages.sendMedia echoes a document carrying the same file id.
func InstallMedia(s *Server) {
	prev := s.responder
	var mu sync.Mutex
	parts := map[int64]map[int32][]byte{}
	big := map[int64]bool{}

	uploaded := func(fileID int64) []byte {
		var out []byte
		for i := int32(0); ; i++ {
			p, ok := parts[fileID][i]
			if !ok {
				break
			}
			out = append(out, p...)
		}
		return out
	}

	s.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
		}
		switch o := obj.(type) {
		case *gen.UploadSaveFilePartRequest:
			mu.Lock()
			if parts[o.FileID] == nil {
				parts[o.FileID] = map[int32][]byte{}
			}
			parts[o.FileID][o.FilePart] = o.Bytes
			mu.Unlock()
			return &gen.BoolTrue{}, nil

		case *gen.UploadSaveBigFilePartRequest:
			mu.Lock()
			if parts[o.FileID] == nil {
				parts[o.FileID] = map[int32][]byte{}
			}
			parts[o.FileID][o.FilePart] = o.Bytes
			big[o.FileID] = true
			mu.Unlock()
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
			mu.Lock()
			data := uploaded(locID)
			mu.Unlock()
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
			return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Mtime: 1700000000,
				Bytes: chunk}, nil

		case *gen.MessagesSendMediaRequest:
			var fileID int64
			if im, ok := o.Media.(*gen.InputMediaUploadedDocument); ok {
				switch f := im.File.(type) {
				case *gen.InputFile:
					fileID = f.ID
				case *gen.InputFileBig:
					fileID = f.ID
				}
			}
			mu.Lock()
			data := uploaded(fileID)
			mu.Unlock()
			size := int64(len(data))
			doc := &gen.Document{
				ID: fileID, AccessHash: demoFileHash,
				FileReference: []byte{1, 2, 3}, Date: 100,
				MimeType: "application/octet-stream", Size: size,
				Attributes: []gen.DocumentAttributeClass{
					&gen.DocumentAttributeFilename{FileName: "demo.bin"},
				},
			}
			m := &gen.Message{ID: 20, Out: true,
				PeerID: peerWire(o.Peer), Date: 100,
				Media: &gen.MessageMediaDocument{
					Document:   doc,
					TTLSeconds: int32p(0),
				}}
			return &gen.Updates{
				Updates: []gen.UpdateClass{
					&gen.UpdateMessageID{RandomID: o.RandomID, ID: 20},
					&gen.UpdateNewMessage{Message: m, PTS: 20, PTSCount: 1},
				},
				Users: []gen.UserClass{selfUser()}, Chats: []gen.ChatClass{},
				Date: 100, Seq: 0,
			}, nil
		}
		return prev(ctor, body)
	})
}

func peerWire(p gen.InputPeerClass) gen.PeerClass {
	switch v := p.(type) {
	case *gen.InputPeerUser:
		return &gen.PeerUser{UserID: v.UserID}
	case *gen.InputPeerChat:
		return &gen.PeerChat{ChatID: v.ChatID}
	case *gen.InputPeerChannel:
		return &gen.PeerChannel{ChannelID: v.ChannelID}
	}
	return &gen.PeerUser{UserID: demoSelfID}
}

func int32p(v int32) *int32 { return &v }
