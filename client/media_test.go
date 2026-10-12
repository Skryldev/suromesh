package client_test

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G401 — protocol-required part checksum verification
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// newMediaClient wires a ready client against the fake app server extended
// with the media RPC family. A non-nil tcfg overrides the transfer config.
func newMediaClient(t testing.TB, tcfg *client.TransferConfig) (*client.Client, *appFake, *mediaFake, *wstest.Server) {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	key := testAuthKeyOf(9)
	cfg := client.Config{
		Session:    session.NewWithKey(key),
		Host:       srv.Addr,
		TLSEnabled: boolPtr(false),
	}
	if tcfg != nil {
		cfg.Transfer = *tcfg
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake := wireFakeAppServer(t, srv, key)
	// Media tests move megabytes; peer-side frame retention would hold
	// every upload frame and poison memory audits.
	fake.SetFrameLog(false)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	mf := newMediaFake()
	installMediaHooks(t, fake, mf)
	return c, fake, mf, srv
}

// patternByte returns the deterministicReader byte at position pos.
func patternByte(pos int64) byte { return byte(pos % 251) }

// patternSlice materializes n pattern bytes (test-only allocation).
func patternSlice(n int64) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = patternByte(int64(i))
	}
	return b
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b) // #nosec G401 — protocol-required
	return hex.EncodeToString(sum[:])
}

const (
	mediaCtorSavePart   = 0xb304a621
	mediaCtorSaveBig    = 0xde7b673d
	mediaCtorGetFile    = 0xbe5335be
	mediaCtorSendMedia  = 0x72ccc23d
	uploadSmallPartSize = 128 * 1024
)

func isMediaCtor(ctor uint32) bool {
	return ctor == mediaCtorSavePart || ctor == mediaCtorSaveBig || ctor == mediaCtorGetFile
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	data := patternSlice(300 * 1024) // 3 parts of 128 KiB
	up, err := c.Upload(context.Background(), bytes.NewReader(data), client.UploadOptions{
		FileName: "roundtrip.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.IsBig {
		t.Fatal("300KB upload must use the small-file path")
	}
	if up.Parts != 3 {
		t.Fatalf("Parts = %d, want 3", up.Parts)
	}
	if up.MD5Sum != md5Hex(data) {
		t.Fatalf("MD5 mismatch: got %s want %s", up.MD5Sum, md5Hex(data))
	}
	if up.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", up.Size, len(data))
	}
	if got := mf.uploadedFile(up.ID, up.Parts); !bytes.Equal(got, data) {
		t.Fatalf("server-side reassembly mismatch (%d bytes)", len(got))
	}
	// Small files carry the MD5 in the InputFile form (protocol rule).
	inf, ok := up.AsInputFile().(*gen.InputFile)
	if !ok || inf.MD5Checksum != up.MD5Sum {
		t.Fatalf("AsInputFile = %T, want InputFile with MD5", up.AsInputFile())
	}

	// Download the same content back through getFile.
	const docID = 42
	mf.setDownloadData(docID, data)
	var out bytes.Buffer
	if err := c.Download(context.Background(), mediaDocLocation(docID, 0xabc), &out, client.DownloadOptions{
		Size: int64(len(data)),
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("download round-trip mismatch: got %d bytes", out.Len())
	}
}

func TestUploadLargeFileBigPart(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	// 10 MiB + 1 byte: over the reference's big-file threshold. The
	// deterministic reader keeps the test's memory bounded.
	const size = 10*1024*1024 + 1
	up, err := c.Upload(context.Background(), newDeterministicReader(size), client.UploadOptions{
		FileName: "big.bin",
		Size:     size,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !up.IsBig {
		t.Fatal("10MiB+ upload must use the big-file path")
	}
	if up.MD5Sum != "" {
		t.Fatalf("big files carry no MD5, got %s", up.MD5Sum)
	}
	partSize := int64(uploadSmallPartSize)
	wantParts := int32((size + partSize - 1) / partSize)
	if up.Parts != wantParts {
		t.Fatalf("Parts = %d, want %d", up.Parts, wantParts)
	}
	if _, ok := up.AsInputFile().(*gen.InputFileBig); !ok {
		t.Fatalf("AsInputFile = %T, want InputFileBig", up.AsInputFile())
	}

	// Verify the fake stored every part via saveBigFilePart with the
	// expected pattern (spot-check first, middle, last — no full copy).
	if !mf.bigParts[up.ID] {
		t.Fatal("fake did not record a saveBigFilePart request")
	}
	mf.mu.Lock()
	pm := mf.parts[up.ID]
	mf.mu.Unlock()
	if len(pm) != int(wantParts) {
		t.Fatalf("fake stored %d parts, want %d", len(pm), wantParts)
	}
	checkPart := func(idx int32) {
		t.Helper()
		part := pm[idx]
		wantLen := partSize
		if int64(idx) == int64(wantParts-1) {
			wantLen = size - int64(wantParts-1)*partSize
		}
		if int64(len(part)) != wantLen {
			t.Fatalf("part %d: %d bytes, want %d", idx, len(part), wantLen)
		}
		for j, b := range part {
			if b != patternByte(int64(idx)*partSize+int64(j)) {
				t.Fatalf("part %d byte %d corrupt", idx, j)
			}
		}
	}
	checkPart(0)
	checkPart(wantParts / 2)
	checkPart(wantParts - 1)
}

func TestUploadZeroBytes(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	up, err := c.Upload(context.Background(), bytes.NewReader(nil), client.UploadOptions{
		FileName: "empty.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	if up.Parts != 0 {
		t.Fatalf("empty file uploaded %d parts, want 0", up.Parts)
	}
	if up.MD5Sum != md5Hex(nil) {
		t.Fatalf("empty MD5 = %s, want %s", up.MD5Sum, md5Hex(nil))
	}
	mf.mu.Lock()
	n := len(mf.parts[up.ID])
	mf.mu.Unlock()
	if n != 0 {
		t.Fatalf("fake stored %d parts for an empty upload", n)
	}
}

// plainReader implements neither Len() nor io.Seeker (size unknown).
type plainReader struct{ r io.Reader }

func (p plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func TestUploadSizeUnknown(t *testing.T) {
	c, _, _, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	_, err := c.Upload(context.Background(), plainReader{r: bytes.NewReader([]byte("hi"))},
		client.UploadOptions{})
	if !errors.Is(err, client.ErrSizeUnknown) {
		t.Fatalf("err = %v, want ErrSizeUnknown", err)
	}
}

func TestInvalidPartSizes(t *testing.T) {
	c, _, _, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	data := []byte("x")
	if _, err := c.Upload(context.Background(), bytes.NewReader(data), client.UploadOptions{
		Size: 1, PartSizeKB: 513, // > 512 cap (reference uploads.py:677)
	}); !errors.Is(err, client.ErrInvalidPartSize) {
		t.Fatalf("513KB upload: err = %v, want ErrInvalidPartSize", err)
	}
	if _, err := c.Upload(context.Background(), bytes.NewReader(data), client.UploadOptions{
		Size: 1, PartSizeKB: -1,
	}); !errors.Is(err, client.ErrInvalidPartSize) {
		t.Fatalf("negative KB upload: err = %v, want ErrInvalidPartSize", err)
	}
	if err := c.Download(context.Background(), mediaDocLocation(1, 1), io.Discard, client.DownloadOptions{
		PartSizeKB: 3, // not divisible by 4096 (downloads.py:551)
	}); !errors.Is(err, client.ErrInvalidPartSize) {
		t.Fatalf("3KB download: err = %v, want ErrInvalidPartSize", err)
	}
}

func TestUploadPartFailureInjection(t *testing.T) {
	c, fake, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	data := patternSlice(300 * 1024) // 3 parts
	var mu sync.Mutex
	mediaRPCs := 0
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if isMediaCtor(ctor) {
			mu.Lock()
			mediaRPCs++
			fail := mediaRPCs == 2 // the second part upload fails
			mu.Unlock()
			if fail {
				return nil, &rpc.RPCError{Code: 400, Message: "FILE_PART_INVALID"}
			}
			return mf.handleMediaRPC(t, body)
		}
		return mf.handleMediaRPC(t, body)
	})

	_, err := c.Upload(context.Background(), bytes.NewReader(data), client.UploadOptions{
		FileName: "fail.bin",
	})
	var perr *client.ErrPartUploadFailed
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v (%T), want ErrPartUploadFailed", err, err)
	}
	if perr.Part != 1 {
		t.Fatalf("failed part = %d, want 1", perr.Part)
	}
	// The failing part was retried once (reader is replayable) and failed
	// both times: only part 0 is stored.
	mf.mu.Lock()
	stored := 0
	for _, pm := range mf.parts {
		stored += len(pm)
	}
	mf.mu.Unlock()
	if stored != 1 {
		t.Fatalf("fake stored %d parts total, want 1", stored)
	}
	mu.Lock()
	calls := mediaRPCs
	mu.Unlock()
	// RPC-level rejections are deterministic protocol failures, not
	// transient network errors: the part is NOT retried (2 RPCs = part 0
	// once + part 1 once).
	if calls != 2 {
		t.Fatalf("media RPCs = %d, want 2", calls)
	}
}

func TestDownloadChunkFailureInjection(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	const docID = 77
	data := patternSlice(512 * 1024) // 4 chunks of 128 KiB
	mf.setDownloadData(docID, data)
	mf.failChunkAt(docID, 3) // the third getFile fails

	var out bytes.Buffer
	err := c.Download(context.Background(), mediaDocLocation(docID, 0x1), &out, client.DownloadOptions{
		Size: int64(len(data)),
	})
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("err = %v (%T), want RPCError", err, err)
	}
	if out.Len() != 2*uploadSmallPartSize {
		t.Fatalf("wrote %d bytes before failure, want %d", out.Len(), 2*uploadSmallPartSize)
	}
	// The unsized path fails the same way (short-chunk EOF never fakes
	// success over a server error). Reset the fake's call counter so the
	// injected failure applies to the fresh download.
	mf.resetCounter(docID)
	out.Reset()
	if err := c.Download(context.Background(), mediaDocLocation(docID, 0x1), &out, client.DownloadOptions{}); err == nil {
		t.Fatal("unsized download succeeded despite injected failure")
	}
}

// failingReader serves its data then fails with a non-EOF error (a reader
// that dies mid-file rather than reaching a clean EOF).
type failingReader struct {
	data []byte
	pos  int
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, f.err
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

func TestReaderErrorTerminatesUpload(t *testing.T) {
	c, _, _, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	boom := errors.New("injected read failure")
	r := &failingReader{data: patternSlice(uploadSmallPartSize), err: boom}
	_, err := c.Upload(context.Background(), r, client.UploadOptions{
		Size: 300 * 1024, FileName: "boom.bin",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped reader error", err)
	}
	// The transfer is marked failed, not completed.
	m := c.TransferMetrics()
	if m.Failed == 0 {
		t.Fatal("failed metric not recorded")
	}
}

func TestUploadCancellation(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	const size = 4 * 1024 * 1024 // 32 parts of 128 KiB
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progressed := make(chan struct{}, 1)
	_, err := c.Upload(ctx, newDeterministicReader(size), client.UploadOptions{
		Size: size,
		Progress: func(p client.TransferProgress) {
			select {
			case progressed <- struct{}{}:
				cancel() // cancel after the first part completes
			default:
			}
		},
	})
	<-progressed
	if err == nil {
		t.Fatal("cancelled upload succeeded")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("cancel")) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	mf.mu.Lock()
	stored := 0
	for _, pm := range mf.parts {
		stored += len(pm)
	}
	mf.mu.Unlock()
	if stored >= 32 {
		t.Fatalf("fake stored %d parts after early cancellation", stored)
	}
	m := c.TransferMetrics()
	if m.Cancelled == 0 {
		t.Fatal("cancelled metric not recorded")
	}
}

func TestDownloadCancellation(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	const docID = 91
	data := patternSlice(1024 * 1024) // 8 chunks of 128 KiB
	mf.setDownloadData(docID, data)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progressed := make(chan struct{}, 1)
	var out bytes.Buffer
	err := c.Download(ctx, mediaDocLocation(docID, 0x2), &out, client.DownloadOptions{
		Size: int64(len(data)),
		Progress: func(p client.TransferProgress) {
			select {
			case progressed <- struct{}{}:
				cancel()
			default:
			}
		},
	})
	<-progressed
	if err == nil {
		t.Fatal("cancelled download succeeded")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("cancel")) {
		t.Fatalf("err = %v, want cancellation", err)
	}
	if out.Len() >= len(data) {
		t.Fatalf("download wrote %d bytes despite cancellation", out.Len())
	}
}

func TestDownloadOffsetAlignment(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	const docID = 55
	data := patternSlice(512 * 1024)
	mf.setDownloadData(docID, data)

	// Unaligned offset, unknown size: output must be exactly data[100000:]
	// (reference downloads.py subtracts offset % limit and skips the
	// prefix).
	var out bytes.Buffer
	if err := c.Download(context.Background(), mediaDocLocation(docID, 0x3), &out, client.DownloadOptions{
		Offset: 100000,
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data[100000:]) {
		t.Fatalf("unaligned offset mismatch: got %d bytes, want %d", out.Len(), len(data)-100000)
	}

	// Unaligned offset with a bounded window: exactly Size bytes from the
	// offset even though the final limit is rounded up to 4 KiB.
	out.Reset()
	if err := c.Download(context.Background(), mediaDocLocation(docID, 0x3), &out, client.DownloadOptions{
		Offset: 100000,
		Size:   1000,
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data[100000:101000]) {
		t.Fatalf("windowed download mismatch: got %d bytes, want 1000", out.Len())
	}
}

func TestDownloadSizeIntegrity(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	const docID = 56
	data := patternSlice(128 * 1024)
	mf.setDownloadData(docID, data)

	// Declared size larger than the served file: integrity failure.
	var out bytes.Buffer
	err := c.Download(context.Background(), mediaDocLocation(docID, 0x4), &out, client.DownloadOptions{
		Size: 400 * 1024,
	})
	if !errors.Is(err, client.ErrTransferIntegrity) {
		t.Fatalf("err = %v, want ErrTransferIntegrity", err)
	}
	if out.Len() != len(data) {
		t.Fatalf("wrote %d bytes, want the full %d served bytes", out.Len(), len(data))
	}
}

// gatedReader invokes onFirst exactly once (on the first Read) before
// serving its data. Used to observe when a transfer becomes active.
type gatedReader struct {
	data    []byte
	pos     int
	onFirst func()
	once    sync.Once
}

func (g *gatedReader) Read(p []byte) (int, error) {
	g.once.Do(g.onFirst)
	if g.pos >= len(g.data) {
		return 0, io.EOF
	}
	n := copy(p, g.data[g.pos:])
	g.pos += n
	return n, nil
}

func TestConcurrentTransfersBounded(t *testing.T) {
	c, _, mf, srv := newMediaClient(t, &client.TransferConfig{
		MaxConcurrentTransfers: 4,
	})
	defer srv.Close()
	defer c.Close()

	// 12 uploads against 4 transfer slots. Every upload gates inside its
	// first Read: an upload can only reach Read after acquiring a slot, so
	// the gate directly observes how many slots are held concurrently.
	release := make(chan struct{})
	arrived := make(chan struct{}, 12)
	var wg sync.WaitGroup
	errs := make([]error, 12)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := &gatedReader{data: patternSlice(64 * 1024), onFirst: func() {
				arrived <- struct{}{}
				<-release
			}}
			_, errs[i] = c.Upload(context.Background(), r, client.UploadOptions{
				Size: 64 * 1024,
			})
		}(i)
	}

	// Exactly MaxConcurrentTransfers uploads become active.
	for i := 0; i < 4; i++ {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 4 expected transfers became active", i)
		}
	}
	// The remaining 8 must still be queued on the semaphore.
	select {
	case <-arrived:
		t.Fatal("more than MaxConcurrentTransfers transfers became active")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	if m := c.TransferMetrics(); m.Completed != 12 {
		t.Fatalf("completed = %d, want 12", m.Completed)
	}
	_ = mf
}

func TestCloseDuringUpload(t *testing.T) {
	c, fake, _, srv := newMediaClient(t, nil)
	fake.SetDelay(30 * time.Millisecond)

	before := runtime.NumGoroutine()
	done := make(chan error, 1)
	go func() {
		_, err := c.Upload(context.Background(), newDeterministicReader(2*1024*1024), client.UploadOptions{
			Size: 2 * 1024 * 1024,
		})
		done <- err
	}()
	time.Sleep(80 * time.Millisecond) // let the upload start
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("upload succeeded after Close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("upload did not settle after Close")
	}
	// Goroutines settle after close (bounded tolerance for runtime churn).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+4 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+8 {
		t.Fatalf("goroutines grew %d → %d after close-during-upload", before, after)
	}
	srv.Close()
}

func TestSendMediaRoundTrip(t *testing.T) {
	c, fake, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	data := patternSlice(150 * 1024) // 2 parts
	const docID, docHash = 777, int64(0xbeef)

	// Handle messages.sendMedia: capture the uploaded InputFile, register
	// its content under the document id, and answer with Updates carrying
	// the sent message.
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch {
		case isMediaCtor(ctor):
			return mf.handleMediaRPC(t, body)
		case ctor == mediaCtorSendMedia:
			obj, err := tl.Unmarshal(body)
			if err != nil {
				return nil, &rpc.RPCError{Code: 400, Message: "BAD"}
			}
			req, ok := obj.(*gen.MessagesSendMediaRequest)
			if !ok {
				return nil, &rpc.RPCError{Code: 400, Message: fmt.Sprintf("decoded %T, want MessagesSendMediaRequest", obj)}
			}
			media, ok := req.Media.(*gen.InputMediaUploadedDocument)
			if !ok {
				return nil, &rpc.RPCError{Code: 400, Message: fmt.Sprintf("media %T, want InputMediaUploadedDocument", req.Media)}
			}
			var fileID int64
			switch f := media.File.(type) {
			case *gen.InputFile:
				fileID = f.ID
				if f.MD5Checksum == "" {
					return nil, &rpc.RPCError{Code: 400, Message: "small-file InputFile missing MD5"}
				}
			case *gen.InputFileBig:
				fileID = f.ID
			default:
				return nil, &rpc.RPCError{Code: 400, Message: fmt.Sprintf("file %T", media.File)}
			}
			sent := mf.uploadedFile(fileID, 2)
			if !bytes.Equal(sent, data) {
				return nil, &rpc.RPCError{Code: 400, Message: "sendMedia received wrong file content"}
			}
			mf.setDownloadData(docID, sent)
			return &gen.Updates{
				Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
					Message: &gen.Message{
						ID:     9,
						PeerID: &gen.PeerChat{ChatID: 100},
						Media: &gen.MessageMediaDocument{
							Document: &gen.Document{
								ID:         docID,
								AccessHash: docHash,
								MimeType:   media.MimeType,
								Size:       int64(len(sent)),
							},
						},
					},
				}},
				Date: 1700000000,
			}, nil
		}
		return mf.handleMediaRPC(t, body)
	})

	msg, err := c.SendMedia(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 100},
		bytes.NewReader(data), client.UploadOptions{FileName: "send.bin"})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != 9 {
		t.Fatalf("sent message ID = %d, want 9", msg.ID)
	}
	if msg.Raw == nil {
		t.Fatal("message carries no Raw form for media extraction")
	}

	// DownloadMedia closes the loop: message → location → getFile.
	var out bytes.Buffer
	if err := c.DownloadMedia(context.Background(), msg, &out, client.DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("DownloadMedia content mismatch: %d bytes", out.Len())
	}
}

func TestDownloadMediaUnsupported(t *testing.T) {
	c, _, _, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	if err := c.DownloadMedia(context.Background(), &client.Message{}, io.Discard, client.DownloadOptions{}); err == nil {
		t.Fatal("nil-raw message accepted")
	}
	noMedia := &client.Message{Raw: &gen.Message{}}
	if err := c.DownloadMedia(context.Background(), noMedia, io.Discard, client.DownloadOptions{}); !errors.Is(err, client.ErrUnsupportedMedia) {
		t.Fatalf("err = %v, want ErrUnsupportedMedia", err)
	}
}

func TestProgressPerPart(t *testing.T) {
	c, _, _, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	calls := 0
	data := patternSlice(300 * 1024)
	if _, err := c.Upload(context.Background(), bytes.NewReader(data), client.UploadOptions{
		Progress: func(p client.TransferProgress) {
			calls++
			if p.Kind != client.TransferUpload {
				t.Errorf("progress kind = %v", p.Kind)
			}
			if p.Total != int64(len(data)) {
				t.Errorf("progress total = %d", p.Total)
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("progress fired %d times, want 3 (one per part)", calls)
	}
}

func TestProgressPanicIsolated(t *testing.T) {
	c, _, _, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	data := patternSlice(64 * 1024)
	up, err := c.Upload(context.Background(), bytes.NewReader(data), client.UploadOptions{
		Progress: func(p client.TransferProgress) {
			panic("user callback panic")
		},
	})
	if err != nil {
		t.Fatalf("upload failed after callback panic: %v", err)
	}
	if up == nil || up.Parts != 1 {
		t.Fatalf("upload result = %+v", up)
	}
}
