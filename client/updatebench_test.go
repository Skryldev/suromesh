package client_test

// Phase 10 benchmarks: update pipeline cost per stage.

import (
	"context"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func mkUpd(pts int32) *gen.UpdateNewMessage {
	return &gen.UpdateNewMessage{
		Message: &gen.Message{ID: pts, Message: "bench", PeerID: &gen.PeerUser{UserID: 1}},
		PTS:     pts, PTSCount: 1,
	}
}

func BenchmarkUpdateSingleBatch(b *testing.B) {
	c, fake, _ := newUpdatesClientW(b, 1)
	defer c.Close()

	var got int64
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) { got++ })
	defer sub.Unsubscribe()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{mkUpd(int32(i + 1))},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && got != int64(b.N) {
		time.Sleep(5 * time.Millisecond)
	}
	if got != int64(b.N) {
		b.Fatalf("delivered %d of %d", got, b.N)
	}
}

func BenchmarkUpdate1000Sequential(b *testing.B) {
	c, fake, _ := newUpdatesClientW(b, 1)
	defer c.Close()

	var got int64
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) { got++ })
	defer sub.Unsubscribe()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ups := make([]gen.UpdateClass, 1000)
		for j := range ups {
			ups[j] = mkUpd(int32(i*1000 + j + 1))
		}
		fake.PushUpdate(&gen.Updates{Updates: ups, Users: []gen.UserClass{},
			Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) && got != int64(1000)*int64(b.N) {
		time.Sleep(5 * time.Millisecond)
	}
	if got != int64(1000)*int64(b.N) {
		b.Fatalf("delivered %d", got)
	}
}

func BenchmarkEntityExtraction(b *testing.B) {
	c, fake, _ := newUpdatesClientW(b, 1)
	defer c.Close()

	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {})
	defer sub.Unsubscribe()

	env := &gen.Updates{
		Updates: []gen.UpdateClass{mkUpd(1)},
		Users:   []gen.UserClass{testUser(1, 1, "u")},
		Chats:   []gen.ChatClass{testChannel(2, 2)},
		Date:    100, Seq: 0,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fake.PushUpdate(env)
		// PTS stays at 1 → duplicates after the first; the entity seeding
		// path still runs per batch (the stage under measurement).
	}
}

func BenchmarkSubscriberFanOut(b *testing.B) {
	c, fake, _ := newUpdatesClientW(b, 4)
	defer c.Close()

	const subs = 10
	done := make([]chan struct{}, subs)
	for i := 0; i < subs; i++ {
		d := make(chan struct{}, 1)
		done[i] = d
		c.OnUpdate(func(ctx context.Context, upd tl.Object) { d <- struct{}{} })
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{mkUpd(int32(i + 1))},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
		for j := 0; j < subs; j++ {
			<-done[j]
		}
	}
}
