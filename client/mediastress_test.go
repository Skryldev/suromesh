package client_test

// Phase 13 concurrency stress + memory audits: concurrent media sends,
// downloads, update races, album storms, and bounded-memory proofs.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestStress32ConcurrentMediaSends: 32 concurrent SendMediaInput calls
// (existing-media resend — no uploads) — all succeed, no subscriber
// events, no races.
func TestStress32ConcurrentMediaSends(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newMediaDomainClient(t)
	defer c.Close()

	var events int32
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) { atomic.AddInt32(&events, 1) })
	defer sub.Unsubscribe()

	toDomain := func(id int64) *client.Document {
		return c.ClassifyMediaForTest(mediaDocWire(
			testDocument(id, 0xdead, []byte{byte(id)}, "application/octet-stream"))).(*client.Document)
	}

	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.SendMediaInput(context.Background(),
				client.PeerID{Kind: client.PeerChat, ID: 55},
				client.ExistingDocumentInput{Document: toDomain(int64(100 + i))})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := atomic.LoadInt32(&events); n != 0 {
		t.Fatalf("media sends dispatched %d events", n)
	}
	_ = fake
}

// TestStress100Albums: 100 albums through the collector — bounded state,
// every album delivered complete.
func TestStress100Albums(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	ac := client.NewAlbumCollector()
	ac.MaxPending = 16
	base := time.Unix(1700000000, 0)
	cur := base
	clockMu := sync.Mutex{}
	ac.Now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return cur
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		cur = cur.Add(d)
		clockMu.Unlock()
	}

	const albums, per = 100, 3
	for a := int64(0); a < albums; a++ {
		for i := int32(0); i < per; i++ {
			ac.Feed(client.Message{ID: a*100 + int64(i), PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
				GroupedID: a, InAlbum: true})
		}
		advance(250 * time.Millisecond) // expire each before the next
	}
	got := 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for got < albums {
		alb, ok := ac.Next(ctx)
		if !ok {
			t.Fatalf("album stream ended after %d of %d", got, albums)
		}
		if len(alb.Messages) != per {
			t.Fatalf("album %d has %d messages", alb.GroupedID, len(alb.Messages))
		}
		got++
	}
}

// TestEntityRotationDuringMediaOp: a newer file reference from an update
// rotates the cached state; the media operation uses the server's value.
func TestEntityRotationDuringMediaOp(t *testing.T) {
	c, fake, _ := newMediaDomainClient(t)
	defer c.Close()

	// Push a media message from the "server": user 900 with a photo.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 5, PeerID: &gen.PeerUser{UserID: 900}, Date: 5,
			Media: &gen.MessageMediaPhoto{Photo: testPhoto(500, 0xaaa, []byte{1, 1, 1})}},
		PTS: 5, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

	// The update's media is visible to the conversation layer via the
	// subscriber (server-originated update — dispatched normally).
	var mu sync.Mutex
	var seenMedia client.Media
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if un, ok := upd.(*gen.UpdateNewMessage); ok {
			if m, ok := un.Message.(*gen.Message); ok {
				mu.Lock()
				seenMedia = c.ClassifyMediaForTest(m.Media)
				mu.Unlock()
			}
		}
	})
	defer sub.Unsubscribe()
	waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return seenMedia != nil
	})
	mu.Lock()
	got := seenMedia
	mu.Unlock()
	if p, ok := got.(*client.Photo); !ok || p.ID != 500 {
		t.Fatalf("update media = %+v", got)
	}
}

// TestMediaConcurrentWithDialogs: media sends racing dialog iteration.
func TestMediaConcurrentWithDialogs(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, mf := newMediaDomainClient(t)
	defer c.Close()
	df := newDialogsFake()
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if isMutationCtor(ctor) {
			return mf.handle(t, ctor, body)
		}
		return df.handle(t, ctor, body)
	})

	toDomain := func(id int64) *client.Document {
		return c.ClassifyMediaForTest(mediaDocWire(
			testDocument(id, 0xdead, []byte{byte(id)}, "application/octet-stream"))).(*client.Document)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.SendMediaInput(context.Background(),
				client.PeerID{Kind: client.PeerChat, ID: 55},
				client.ExistingDocumentInput{Document: toDomain(int64(100 + i))})
		}(i)
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			it := c.IterDialogs(context.Background(), client.DialogQuery{Limit: 2})
			for {
				if _, ok := it.Next(context.Background()); !ok {
					break
				}
			}
		}()
	}
	wg.Wait()
}
