package tl_test

import (
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// Benchmark fixtures: representative schema shapes — a flags-heavy object, a
// deep Updates tree, and a typical RPC request.

func benchUser() *gen.User {
	ah := int64(987654321)
	fn := "Ali"
	un := "ali"
	return &gen.User{
		ID:         1234567,
		UserType:   &gen.UserTypeNormal{},
		AccessHash: &ah,
		FirstName:  &fn,
		Username:   &un,
	}
}

func benchUpdates() *gen.Updates {
	msg := &gen.Message{
		ID:      7,
		PeerID:  &gen.PeerChat{ChatID: 555},
		Date:    1700000000,
		Message: "hello",
	}
	media := &gen.MessageMediaDocument{Document: &gen.Document{
		ID: 1 << 61, AccessHash: 42, FileReference: []byte{0xDE, 0xAD},
		Date: 1700000000, MimeType: "video/mp4", Size: 1 << 20, DCID: 2,
		Attributes: []gen.DocumentAttributeClass{&gen.DocumentAttributeFilename{FileName: "clip.mp4"}},
	}}
	_ = media
	updates := make([]gen.UpdateClass, 0, 10)
	for i := 0; i < 10; i++ {
		updates = append(updates, &gen.UpdateNewMessage{Message: msg, PTS: int32(i), PTSCount: 1})
	}
	return &gen.Updates{Updates: updates, Users: []gen.UserClass{benchUser()}, Chats: []gen.ChatClass{}, Date: 1700000000, Seq: 3}
}

func benchRequest() *gen.MessagesSendMessageRequest {
	return &gen.MessagesSendMessageRequest{
		NoWebpage: true,
		Peer:      &gen.InputPeerUser{UserID: 111, AccessHash: 222},
		Message:   "سلام دنیا",
		RandomID:  987654321,
	}
}

func BenchmarkEncodeUser(b *testing.B) {
	u := benchUser()
	w := tl.NewWriter(256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		if err := u.Encode(w); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeUser(b *testing.B) {
	raw, _ := tl.Marshal(benchUser())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tl.Unmarshal(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeUpdates(b *testing.B) {
	u := benchUpdates()
	w := tl.NewWriter(4096)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		if err := u.Encode(w); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeUpdates(b *testing.B) {
	raw, _ := tl.Marshal(benchUpdates())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tl.Unmarshal(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeSendMessageRequest(b *testing.B) {
	r := benchRequest()
	w := tl.NewWriter(256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		if err := r.Encode(w); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeSendMessageRequest(b *testing.B) {
	raw, _ := tl.Marshal(benchRequest())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := tl.Unmarshal(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriterPrimitives(b *testing.B) {
	w := tl.NewWriter(128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		w.PutUint32(0x12345678)
		w.PutInt64(-1234567890)
		w.PutString("benchmark string")
		w.PutBoolBoxed(true)
	}
}
