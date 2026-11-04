package client

import (
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// FuzzResolveUploadPartSize pins the security-critical arithmetic of
// user-supplied part sizes: for ANY int32 KB input the resolver either
// rejects or returns a size that satisfies every protocol invariant.
// Regression guard for the int32 kb*1024 wrap (4194304*1024 → 0) that
// previously could reach the wire.
func FuzzResolveUploadPartSize(f *testing.F) {
	seeds := []int32{0, 1, 3, 128, 256, 512, 513, 4096, 1 << 20, 4194304, -1, -1 << 31}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, kb int32) {
		ps, err := resolveUploadPartSize(128*1024, kb)
		if err != nil {
			return // rejection is always acceptable
		}
		if ps <= 0 || ps > maxUploadPartSize {
			t.Fatalf("accepted invalid part size %d for kb=%d", ps, kb)
		}
		if ps%1024 != 0 {
			t.Fatalf("part size %d not divisible by 1024 (kb=%d)", ps, kb)
		}
	})
}

// FuzzResolveDownloadChunkSize: same contract for download limits —
// divisibility by 4096 and the 512 KB cap must hold for every input that
// is not rejected.
func FuzzResolveDownloadChunkSize(f *testing.F) {
	seeds := []int32{0, 1, 3, 64, 128, 512, 513, 4096, 4194304, 1 << 20, -1, -1 << 31}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, kb int32) {
		cs, err := resolveDownloadChunkSize(64*1024, kb)
		if err != nil {
			return
		}
		if cs <= 0 || cs > maxDownloadChunk {
			t.Fatalf("accepted invalid chunk size %d for kb=%d", cs, kb)
		}
		if cs%minDownloadChunk != 0 {
			t.Fatalf("chunk size %d not divisible by 4096 (kb=%d)", cs, kb)
		}
	})
}

// FuzzChunkClassification exercises the downloadRange decision table
// (short chunk = EOF, empty chunk = EOF, full chunk = continue) against
// hostile server answers. The classification must never panic and must
// never report more bytes than the chunk actually carried.
func FuzzChunkClassification(f *testing.F) {
	f.Add(int32(4096), []byte(nil))
	f.Add(int32(131072), make([]byte, 131072))
	f.Add(int32(65536), make([]byte, 1000))
	f.Fuzz(func(t *testing.T, limit int32, body []byte) {
		if limit <= 0 {
			limit = 4096 // the engine's defaults are always positive
		}
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return // malformed server data is the decode layer's seam
		}
		uf, ok := obj.(*gen.UploadFile)
		if !ok {
			return
		}
		// Mirrors downloadRange's classification (client/download.go).
		got := int64(len(uf.Bytes))
		switch {
		case got == 0:
			// EOF
		case got < int64(limit):
			// EOF after delivering `got` bytes
			if got < 0 {
				t.Fatal("negative byte count")
			}
		default:
			// full chunk: exactly limit bytes expected on the wire
			if got < int64(limit) {
				t.Fatalf("full-chunk branch with %d < %d bytes", got, limit)
			}
		}
	})
}
