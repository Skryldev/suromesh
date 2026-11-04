package client_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
)

// heapAfterGC returns HeapAlloc after forcing GC (two passes for
// finalizer/queue lag).
func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// goroutineSettled waits (bounded) for the goroutine count to drop to
// base+tolerance.
func goroutineSettled(base, tolerance int) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= base+tolerance {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("goroutines did not settle: base %d, now %d", base, runtime.NumGoroutine())
}

// TestStress100ConcurrentTransfers: 100 mixed transfers against a client
// bounded at 32 concurrent slots. Every transfer must complete, counters
// must reconcile exactly, and goroutines must not leak.
func TestStress100ConcurrentTransfers(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test (short mode)")
	}
	c, _, mf, srv := newMediaClient(t, &client.TransferConfig{MaxConcurrentTransfers: 32})
	defer srv.Close()
	defer c.Close()
	mf.enableDiscard()

	const uploads, downloads = 50, 50
	const upSize = 128 * 1024
	const dnSize = 256 * 1024
	for i := 0; i < downloads; i++ {
		mf.setSynth(1000+int64(i), dnSize)
	}

	base := runtime.NumGoroutine()
	var wg sync.WaitGroup
	var failures atomic.Int64
	start := time.Now()
	for i := 0; i < uploads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Upload(context.Background(), newDeterministicReader(upSize),
				client.UploadOptions{Size: upSize}); err != nil {
				failures.Add(1)
				t.Errorf("upload %d: %v", i, err)
			}
		}(i)
	}
	for i := 0; i < downloads; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := c.Download(context.Background(), mediaDocLocation(1000+int64(i), int64(i)),
				io.Discard, client.DownloadOptions{Size: dnSize})
			if err != nil {
				failures.Add(1)
				t.Errorf("download %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)
	if failures.Load() != 0 {
		t.Fatalf("%d transfers failed", failures.Load())
	}

	m := c.TransferMetrics()
	if m.Started != uploads+downloads || m.Completed != uploads+downloads {
		t.Fatalf("metrics: started %d completed %d, want %d/%d",
			m.Started, m.Completed, uploads+downloads, uploads+downloads)
	}
	if m.BytesUp != uploads*upSize {
		t.Fatalf("bytesUp = %d, want %d", m.BytesUp, uploads*upSize)
	}
	if m.BytesDown != downloads*dnSize {
		t.Fatalf("bytesDown = %d, want %d", m.BytesDown, downloads*dnSize)
	}
	if m.Active != 0 {
		t.Fatalf("active = %d after completion, want 0", m.Active)
	}
	t.Logf("100 transfers in %s", elapsed)

	if err := goroutineSettled(base, 8); err != nil {
		t.Fatal(err)
	}
}

// TestStress1000SequentialTransfers: 1000 sequential uploads of one part
// each. Throughput sanity plus exact reconciliation; the transfer manager
// must not accumulate state per transfer.
func TestStress1000SequentialTransfers(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test (short mode)")
	}
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()
	mf.enableDiscard()

	const n = 1000
	const size = 4 * 1024
	heapBefore := heapAfterGC()
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := c.Upload(context.Background(), newDeterministicReader(size),
			client.UploadOptions{Size: size}); err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	t.Logf("1000 sequential uploads in %s", elapsed)

	m := c.TransferMetrics()
	if m.Completed != n {
		t.Fatalf("completed = %d, want %d", m.Completed, n)
	}
	if m.BytesUp != n*size {
		t.Fatalf("bytesUp = %d, want %d", m.BytesUp, n*size)
	}
	if m.Active != 0 {
		t.Fatalf("active = %d, want 0", m.Active)
	}

	// Per-transfer bookkeeping must not accumulate: heap after 1000 tiny
	// uploads stays flat relative to the transfer payload volume.
	heapAfter := heapAfterGC()
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("heap growth over 1000 uploads: %d bytes", growth)
	if growth > 4*1024*1024 {
		t.Fatalf("heap grew %d bytes across 1000 sequential transfers (leak?)", growth)
	}
}

// TestMemoryBoundedLargeUpload: a 64 MiB logical upload through the
// deterministic reader must hold O(part_size) client memory — not
// O(file_size). The fake runs in discard mode so its own allocations stay
// out of the measurement.
func TestMemoryBoundedLargeUpload(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test (short mode)")
	}
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()
	mf.enableDiscard()

	const size = 64 << 20 // 64 MiB logical, never materialized
	heapBefore := heapAfterGC()

	up, err := c.Upload(context.Background(), newDeterministicReader(size),
		client.UploadOptions{Size: size, FileName: "big.bin"})
	if err != nil {
		t.Fatal(err)
	}
	heapAfter := heapAfterGC()
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("client heap growth uploading 64 MiB: %.1f MiB", float64(growth)/(1<<20))

	// Bounded-memory contract: far below file size. The part buffer is
	// 128 KiB; RPC framing and GC churn justify the generous ceiling.
	if growth > 16*1024*1024 {
		t.Fatalf("heap grew %.1f MiB for a 64 MiB upload (not O(part_size))",
			float64(growth)/(1<<20))
	}

	if up.Parts != 512 { // 64 MiB / 128 KiB
		t.Fatalf("Parts = %d, want 512", up.Parts)
	}
	if !up.IsBig {
		t.Fatal("64 MiB upload must use the big-file path")
	}
	if got := mf.uploadedLen(up.ID); got != size {
		t.Fatalf("server accepted %d bytes, want %d", got, size)
	}
	if got := mf.uploadedParts(up.ID); got != 512 {
		t.Fatalf("server accepted %d parts, want 512", got)
	}
	m := c.TransferMetrics()
	if m.BytesUp != size {
		t.Fatalf("bytesUp = %d, want %d", m.BytesUp, size)
	}
}

// TestMemoryBoundedLargeDownload: the same contract for downloads — 64 MiB
// served synthetically, written to io.Discard, one 128 KiB chunk in
// flight at a time.
func TestMemoryBoundedLargeDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test (short mode)")
	}
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	const locID = int64(777)
	const size = 64 << 20
	mf.setSynth(locID, size)
	heapBefore := heapAfterGC()

	if err := c.Download(context.Background(), mediaDocLocation(locID, 1),
		io.Discard, client.DownloadOptions{Size: size}); err != nil {
		t.Fatal(err)
	}
	heapAfter := heapAfterGC()
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("client heap growth downloading 64 MiB: %.1f MiB", float64(growth)/(1<<20))

	if growth > 16*1024*1024 {
		t.Fatalf("heap grew %.1f MiB for a 64 MiB download (not O(chunk_size))",
			float64(growth)/(1<<20))
	}
	m := c.TransferMetrics()
	if m.BytesDown != size {
		t.Fatalf("bytesDown = %d, want %d", m.BytesDown, size)
	}
}

// TestStressConcurrentMixedWithFailures: failures and cancellations racing
// with successes must leave the manager consistent (no stuck slots, exact
// terminal-state accounting).
func TestStressConcurrentMixedWithFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test (short mode)")
	}
	c, fake, mf, srv := newMediaClient(t, &client.TransferConfig{MaxConcurrentTransfers: 16})
	defer srv.Close()
	defer c.Close()
	mf.enableDiscard()

	// Every 4th media RPC fails once at the fake; clients see RPC errors
	// and their uploads fail cleanly.
	var rpcCount atomic.Int64
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if isMediaCtor(ctor) && rpcCount.Add(1)%4 == 0 {
			return nil, &rpc.RPCError{Code: 500, Message: "INJECTED"}
		}
		return mf.handleMediaRPC(t, body)
	})

	const n = 60
	var wg sync.WaitGroup
	var ok, failed atomic.Int64
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Upload(context.Background(), newDeterministicReader(64*1024),
				client.UploadOptions{Size: 64 * 1024})
			if err != nil {
				failed.Add(1)
				return
			}
			ok.Add(1)
		}(i)
	}
	wg.Wait()

	m := c.TransferMetrics()
	if ok.Load()+failed.Load() != n {
		t.Fatalf("outcomes %d+%d != %d", ok.Load(), failed.Load(), n)
	}
	if m.Completed != uint64(ok.Load()) || m.Failed != uint64(failed.Load()) {
		t.Fatalf("metrics mismatch: completed %d vs %d ok, failed %d vs %d",
			m.Completed, ok.Load(), m.Failed, failed.Load())
	}
	if m.Active != 0 {
		t.Fatalf("active = %d, want 0 (slots leaked)", m.Active)
	}
	// The manager still accepts new work after failures.
	if _, err := c.Upload(context.Background(), newDeterministicReader(8*1024),
		client.UploadOptions{Size: 8 * 1024}); err != nil {
		t.Fatalf("upload after failures: %v", err)
	}
}

// TestCancelAllTransfersOnClose: a closed client must settle every
// in-flight transfer with an error.
func TestCancelAllTransfersOnClose(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test (short mode)")
	}
	c, fake, _, srv := newMediaClient(t, &client.TransferConfig{MaxConcurrentTransfers: 8})
	fake.SetDelay(40 * time.Millisecond)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Upload(context.Background(), newDeterministicReader(512*1024),
				client.UploadOptions{Size: 512 * 1024})
		}(i)
	}
	time.Sleep(120 * time.Millisecond) // all 8 transfers become active
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("transfer %d succeeded after Close", i)
		}
	}
	m := c.TransferMetrics()
	if m.Started == 0 || m.Completed != 0 {
		t.Fatalf("metrics after close: started %d, completed %d", m.Started, m.Completed)
	}
	srv.Close()
}

var _ = errors.New
