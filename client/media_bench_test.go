package client_test

import (
	"context"
	"io"
	"testing"

	"github.com/Skryldev/suromesh/client"
)

// Benchmarks run against the in-memory fake transport: they measure the
// client's transfer engine (part loop, framing, encryption, RPC round
// trip), not network throughput.

func BenchmarkUploadSinglePart(b *testing.B) {
	c, _, _, srv := newMediaClient(b, nil)
	defer srv.Close()
	defer c.Close()

	data := patternSlice(64 * 1024)
	b.SetBytes(64 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Upload(context.Background(), &raReader{data}, client.UploadOptions{
			Size: 64 * 1024,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUploadMultiPart(b *testing.B) {
	c, _, _, srv := newMediaClient(b, nil)
	defer srv.Close()
	defer c.Close()

	const size = 2 << 20 // 2 MiB → 16 parts of 128 KiB (big-file path)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Upload(context.Background(), newDeterministicReader(size),
			client.UploadOptions{Size: size}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDownloadSingleChunk(b *testing.B) {
	c, _, mf, srv := newMediaClient(b, nil)
	defer srv.Close()
	defer c.Close()

	const locID = int64(1)
	mf.setSynth(locID, 64*1024)
	b.SetBytes(64 * 1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Download(context.Background(), mediaDocLocation(locID, 1),
			io.Discard, client.DownloadOptions{Size: 64 * 1024}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDownloadMultiChunk(b *testing.B) {
	c, _, mf, srv := newMediaClient(b, nil)
	defer srv.Close()
	defer c.Close()

	const locID = int64(2)
	const size = 2 << 20 // 2 MiB → 16 chunks of 128 KiB
	mf.setSynth(locID, size)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := c.Download(context.Background(), mediaDocLocation(locID, 1),
			io.Discard, client.DownloadOptions{Size: size}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUploadWithProgress(b *testing.B) {
	c, _, _, srv := newMediaClient(b, nil)
	defer srv.Close()
	defer c.Close()

	const size = 512 * 1024 // 4 parts → 4 progress callbacks per upload
	data := patternSlice(size)
	var sink int64
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Upload(context.Background(), &raReader{data}, client.UploadOptions{
			Size: size,
			Progress: func(p client.TransferProgress) {
				sink += p.Current // cheap but real callback work
			},
		}); err != nil {
			b.Fatal(err)
		}
	}
	_ = sink
}

// raReader is a zero-copy reader over an existing buffer.
type raReader struct{ b []byte }

func (r *raReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
