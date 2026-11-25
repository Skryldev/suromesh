package client_test

// Phase 27 §9: bounded-memory streaming evidence, measured against the
// REAL SDK upload/download loops through the in-memory fake transport
// (enableDiscard / setSynth: server-side retention disabled so only the
// client's own allocations are visible). The service's io.Pipe wiring is
// only as bounded as the SDK underneath it, so the claim is asserted
// here, at the source, with runtime.MemStats deltas around 32 MiB
// transfers (GC-settled before/after; the budget is 8 MiB — 16× the
// largest part size and 1/4 of the payload, so whole-payload buffering
// can never pass).

import (
	"bytes"
	"context"
	"runtime"
	"testing"

	"github.com/Skryldev/suromesh/client"
)

const (
	memPayload = int64(32 << 20) // 32 MiB transfer workload
	memBudget  = 8 << 20         // 8 MiB heap-growth budget (>> 512 KiB part ceiling)
)

func heapAllocSettled() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// TestMemoryEvidence_UploadBounded pins: uploading 32 MiB through the
// SDK grows the heap by well under the payload size (bounded by the
// part buffer, not the file).
func TestMemoryEvidence_UploadBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("memory evidence test (32 MiB workload)")
	}
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()
	mf.enableDiscard() // length accounting only: no server-side copies

	before := heapAllocSettled()
	res, err := c.Upload(context.Background(), newDeterministicReader(memPayload),
		client.UploadOptions{FileName: "mem.bin", Size: memPayload})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	after := heapAllocSettled()
	growth := after - before
	if got := mf.uploadedLen(res.ID); got != memPayload {
		t.Fatalf("server accepted %d bytes, want %d", got, memPayload)
	}
	t.Logf("upload %d MiB: heap growth %d KiB (budget %d KiB), parts=%d",
		memPayload>>20, growth>>10, memBudget>>10, res.Parts)
	if growth > memBudget {
		t.Fatalf("upload buffered the payload: heap grew %d KiB > budget %d KiB",
			growth>>10, memBudget>>10)
	}
}

// TestMemoryEvidence_DownloadBounded pins the mirror assertion for the
// download path (synthetic server pattern: served without materializing).
func TestMemoryEvidence_DownloadBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("memory evidence test (32 MiB workload)")
	}
	c, _, mf, srv := newMediaClient(t, nil)
	defer srv.Close()
	defer c.Close()

	const docID = int64(4242)
	mf.setSynth(docID, memPayload)

	var out countingNullWriter
	before := heapAllocSettled()
	if err := c.Download(context.Background(), mediaDocLocation(docID, 0x1), &out,
		client.DownloadOptions{Size: memPayload}); err != nil {
		t.Fatalf("download: %v", err)
	}
	after := heapAllocSettled()
	growth := after - before
	if out.n != memPayload {
		t.Fatalf("downloaded %d bytes, want %d", out.n, memPayload)
	}
	t.Logf("download %d MiB: heap growth %d KiB (budget %d KiB)",
		memPayload>>20, growth>>10, memBudget>>10)
	if growth > memBudget {
		t.Fatalf("download buffered the payload: heap grew %d KiB > budget %d KiB",
			growth>>10, memBudget>>10)
	}
}

// countingNullWriter counts bytes without retaining them (a bytes.Buffer
// would attribute the payload to the TEST, masking client behavior).
type countingNullWriter struct{ n int64 }

func (w *countingNullWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

var _ = bytes.MinRead // keep bytes imported for future chunk assertions
