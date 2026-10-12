package client_test

// Phase 12 benchmarks: mutation + search costs against the fake transport
// (protocol/CPU cost; not network-representative).

import (
	"context"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl/gen"
)

func BenchmarkSendMessage(b *testing.B) {
	c, _, _ := newMutationsClient(b)
	defer c.Close()
	peer := client.PeerID{Kind: client.PeerChat, ID: 55}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.SendMessageOpts(context.Background(), peer, "bench",
			client.SendMessageOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEditMessage(b *testing.B) {
	c, _, _ := newMutationsClient(b)
	defer c.Close()
	peer := client.PeerID{Kind: client.PeerChat, ID: 55}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.EditMessage(context.Background(), peer, 42, "bench"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDeleteMessages(b *testing.B) {
	c, _, _ := newMutationsClient(b)
	defer c.Close()
	peer := client.PeerID{Kind: client.PeerChat, ID: 55}
	ids := []int64{1, 2, 3}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.DeleteMessages(context.Background(), peer, ids,
			client.DeleteMessagesOptions{Revoke: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkForwardMessages(b *testing.B) {
	c, _, _ := newMutationsClient(b)
	defer c.Close()
	peer := client.PeerID{Kind: client.PeerChat, ID: 55}
	dest := client.PeerID{Kind: client.PeerChat, ID: 77}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ForwardMessages(context.Background(), peer, dest,
			[]int64{1, 2, 3}, client.ForwardOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchMessageConversion(b *testing.B) {
	c, _, mf := newMutationsClient(b)
	defer c.Close()
	mf.repeatSearch = true
	mf.pushSearchPage(searchPage(600, 599))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := c.IterSearchMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.SearchQuery{})
		if err != nil {
			b.Fatal(err)
		}
		_, _ = it.Next(context.Background()) // one result per iteration
	}
}

func BenchmarkSearchPagination(b *testing.B) {
	c, _, mf := newMutationsClient(b)
	defer c.Close()
	mf.repeatSearch = true
	mf.pushSearchPage(searchPage(600, 599, 598, 597, 596, 595, 594, 593, 592, 591))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := c.IterSearchMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.SearchQuery{})
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

func BenchmarkConversationMutation(b *testing.B) {
	c, fake, _ := newMutationsClient(b)
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
