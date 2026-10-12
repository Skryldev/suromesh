package client_test

// Phase 11 benchmarks: peer resolution and conversion costs.

import (
	"context"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func BenchmarkResolvePeer(b *testing.B) {
	c, _, _ := newDialogsClient(b)
	defer c.Close()
	c.SeedEntities([]*client.User{{ID: 1, AccessHash: 42}}, nil, nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 1}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEntityToInputPeer(b *testing.B) {
	c, _, _ := newDialogsClient(b)
	defer c.Close()
	ch := &client.Channel{ID: 42, AccessHash: 0xabc}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.InputPeerFor(context.Background(), ch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDialogConversion(b *testing.B) {
	c, _, df := newDialogsClient(b)
	defer c.Close()
	df.repeatLastPage = true
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{&gen.Dialog{Peer: &gen.PeerUser{UserID: 1}, TopMessage: 1,
			NotifySettings: &gen.PeerNotifySettings{}}},
		[]gen.MessageClass{&gen.Message{ID: 1, PeerID: &gen.PeerUser{UserID: 1}, Date: 1}},
		[]gen.UserClass{testUser(1, 1, "a")}, nil, 1, false))
	// Drain once so the entity cache is warm.
	it := c.IterDialogs(context.Background(), client.DialogQuery{Limit: 1})
	for {
		if _, ok := it.Next(context.Background()); !ok {
			break
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it := c.IterDialogs(context.Background(), client.DialogQuery{Limit: 1})
		for {
			if _, ok := it.Next(context.Background()); !ok {
				break
			}
		}
	}
}

func BenchmarkMessageConversion(b *testing.B) {
	c, _, df := newDialogsClient(b)
	defer c.Close()
	df.repeatLastHist = true
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 600, Message: "bench", PeerID: &gen.PeerChat{ChatID: 55}, Date: 600},
	}, 1000000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := c.IterMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
		if err != nil {
			b.Fatal(err)
		}
		_, _ = it.Next(context.Background()) // one message per iteration
	}
}

func BenchmarkHistoryPagination(b *testing.B) {
	c, _, df := newDialogsClient(b)
	defer c.Close()
	df.repeatLastHist = true
	msgs := make([]*gen.Message, 100)
	for i := range msgs {
		msgs[i] = &gen.Message{ID: int32(100000 - i), Message: "p",
			PeerID: &gen.PeerChat{ChatID: 55}, Date: int32(100000 - i)}
	}
	df.pushHistoryPage(histSlicePage(msgs, 1000000))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := c.IterMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, ok := it.Next(context.Background()); !ok {
				break
			}
		}
	}
}

func BenchmarkConversationUpdate(b *testing.B) {
	c, fake, _ := newDialogsClient(b)
	defer c.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: int32(i + 1), PeerID: &gen.PeerUser{UserID: 300}, Date: int32(i + 1)},
			PTS:     int32(i + 1), PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
}

func BenchmarkConversationLookup(b *testing.B) {
	c, fake, _ := newDialogsClient(b)
	defer c.Close()
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 5, PeerID: &gen.PeerUser{UserID: 300}, Date: 5},
		PTS:     5, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	time.Sleep(200 * time.Millisecond)
	peer := client.PeerID{Kind: client.PeerUser, ID: 300}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := c.Conversation(peer); !ok {
			b.Fatal("missing")
		}
	}
}

var _ tl.Object
