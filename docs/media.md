# Media / File Architecture (Phase 8)

The media subsystem implements Soroush Plus file transfer with **bounded
memory**: every engine holds `O(part_size × workers)` bytes — never
`O(file_size)` — regardless of the file being transferred.

Every protocol decision below is traced to the Python reference in
`reference/SPlusthon` (read-only) or to the generated TL constructors
(LAYER 182). Nothing is invented; where the reference is silent (retries,
progress), the behavior is documented as an explicit deviation.

## Module map

| File | Responsibility |
|---|---|
| `client/media_errors.go` | Error sentinels + part-size policy (`partSizeKBForSize`, resolvers) |
| `client/transfer.go` | TransferManager: semaphore, registry, state machine, progress |
| `client/upload.go` | `Client.Upload` — streaming part upload engine |
| `client/download.go` | `Client.Download` — streaming chunk download engine |
| `client/media.go` | Domain `Media`, `SendMedia`, `DownloadMedia` |

## Part sizing (reference parity)

`utils.get_appropriated_part_size` (`reference/SPlusthon/splusthon/utils.py`)
maps file size → part size in KB:

| File size | Part size |
|---|---|
| ≤ 100 MiB | 128 KiB |
| ≤ 750 MiB | 256 KiB |
| > 750 MiB | 512 KiB |

Reference constraints enforced verbatim:

- upload: `part_size % 1024 == 0`, `≤ 512 KB` (`uploads.py:677-683`)
- download: `limit % 4096 == 0`, `≤ 512 KB` (`downloads.py:26-28, 551-553`)
- download default when size unknown: 64 KiB (`downloads.py:546` —
  "Reasonable default")

**Deviation (hardening):** user-supplied `PartSizeKB` is validated with
int64 arithmetic in `resolveUploadPartSize` / `resolveDownloadChunkSize`.
The naive `int32(kb) * 1024` wraps for absurd inputs (4194304 × 1024 → 0),
which would slip past a divisibility check and divide by zero downstream.
The fuzz targets `FuzzResolveUploadPartSize` and
`FuzzResolveDownloadChunkSize` pin the invariant: *reject or satisfy all
protocol constraints, for any int32 input*.

## Upload engine

`Client.Upload(ctx, r io.Reader, opts UploadOptions) (*UploadedFile, error)`

Flow (reference `uploads.py:660-758`):

1. **Size detection** — explicit `opts.Size` > `Len()` > `io.Seeker` >
   error (`ErrSizeUnknown`). The protocol needs the total part count up
   front; the reference gets it from `_FileStream` the same way.
2. **file_id** — `randomInt64()` (reference `helpers.generate_random_long`).
3. **Part loop (sequential)** — the reference uploads strictly sequentially
   (`uploads.py:706`). Parts are idempotent (`file_id`, `file_part`), so a
   single bounded retry is protocol-safe, but ordering is preserved.
4. **Small file (≤ 10 MiB)** — `upload.saveFilePart#b304a621`, MD5
   accumulated over parts, result carries
   `InputFile{id, parts, name, md5}` (`uploads.py:728-741`).
5. **Big file (> 10 MiB)** — `upload.saveBigFilePart#de7b673d` with
   `file_total_parts`, result carries `InputFileBig{id, parts, name}` —
   no MD5 (reference `uploads.py:696-698, 753-754`).
6. **Short reads** — legal only on the last part
   (`uploads.py:717-720`); anything else fails with `ErrReaderShortRead`.
7. **Integrity** — uploaded bytes must equal the declared size
   (`ErrTransferIntegrity`), mirroring the reference's `pos` accounting.

Zero-length files upload zero parts; the MD5 of the empty string is still
produced for small-file protocol form.

**Deviation (retry):** the reference fails a transfer on the first part
error (`uploads.py:750-751`). SuroMesh retries a part exactly once, only
for network-class errors, and only when the reader is an `io.ReadSeeker`
(replay via seek to `part × part_size`). RPC-level rejections are
deterministic protocol answers and are **not** retried. Non-replayable
readers fail with `ErrNonReplayableReader` rather than being silently
re-fed fabricated data.

## Download engine

`Client.Download(ctx, location, w io.Writer, opts DownloadOptions) error`

Flow (reference `downloads.py:535-570`):

1. **Chunk size** — `downloadChunkSize(size)` (128/256/512 KiB by size,
   64 KiB when size unknown), `%4096 == 0`, ≤ 512 KiB.
2. **Offset alignment** — the reference subtracts `offset % limit` before
   fetching and skips the prefix bytes (`downloads.py` `_iter_download`).
   SuroMesh fetches one chunk starting at the aligned offset, discards the
   `bad` prefix, and continues from the first clean offset.
3. **Sequential chunks** — `upload.getFile#be5335be(location, offset,
   limit)` per chunk; ordered writes; offset advances by the requested
   limit.
4. **EOF detection** — a chunk shorter than the limit ends the transfer
   (reference short-chunk rule). An empty chunk is EOF.
5. **Integrity** — when `Size` is known, the written count must equal it
   (`ErrTransferIntegrity`). The final chunk's limit is rounded up to the
   protocol's 4 KiB granularity; writes are truncated to the requested
   window so a server holding more data past the window cannot over-write.

## Transfer manager

`client/transfer.go` — the concurrency owner for all transfers.

- **Bounded concurrency** — `MaxConcurrentTransfers` (default 16) slots on
  a semaphore; `begin()` blocks on the caller's context and returns
  `ErrTransferClosed` after shutdown. Proven by
  `TestConcurrentTransfersBounded` (gate: exactly N uploads reach the
  active window, the rest stay queued).
- **State machine** — `Pending → Running → Completed | Failed | Cancelled`;
  terminal transitions are CAS-once (`transfer.transition`). Exactly one
  terminal state per transfer.
- **Progress** — `TransferProgress{ID, Kind, Current, Total}` delivered
  inline per part/chunk; panic-isolated (`reportProgress` recovers) so a
  user callback can never kill an engine worker; never holds manager locks.
- **Metrics** — started/completed/failed/cancelled, bytesUp/bytesDown,
  upload retries (`Client.TransferMetrics()`).
- **Shutdown** — `Close` closes the stop channel, cancels every active
  transfer's context, and refuses new work (`ErrTransferClosed`). Verified
  by `TestCancelAllTransfersOnClose` and `TestCloseDuringUpload`.

## Messaging integration

`client/media.go`:

- `Client.SendMedia(ctx, peer, r, opts)` — Upload → `InputFile` /
  `InputFileBig` → `InputMediaUploadedDocument` (with
  `DocumentAttributeFilename`) → `messages.sendMedia#72ccc23d` → domain
  `Message`.
- `Client.DownloadMedia(ctx, msg, w, opts)` — extracts the document
  reference from the message's raw TL form (never fabricated: ID and
  access hash come from the server's `Document`), builds
  `InputDocumentFileLocation`, and streams the download. Non-document
  media fails with `ErrUnsupportedMedia`.

## Never-fabricate rules

- `file_id` is the only locally generated identifier (protocol-mandated
  random long). Access hashes, file references, and sizes always come from
  server responses.
- No CDN redirects, secure locations, or photo locations are synthesized:
  unsupported kinds return `ErrUnsupportedLocation` / `ErrUnsupportedMedia`.
- Checksums are protocol-required MD5 only (small-file uploads); no other
  hashing is invented.

## Test inventory

| Test | What it proves |
|---|---|
| `TestUploadDownloadRoundTrip` | small-file path: 3 parts, MD5 parity, byte-exact download |
| `TestUploadLargeFileBigPart` | big-file path: 81 parts, pattern-verified parts, no MD5 |
| `TestUploadZeroBytes` | zero-part edge + empty-string MD5 |
| `TestUploadSizeUnknown` / `TestInvalidPartSizes` | size detection errors; 512 KB cap; %4096 rule; negative/overflowed KB |
| `TestUploadPartFailureInjection` | RPC rejection → `ErrPartUploadFailed`, no fabricated retry |
| `TestDownloadChunkFailureInjection` | injected 500 → clean failure, prefix bytes accounted |
| `TestReaderErrorTerminatesUpload` | reader death surfaces wrapped, transfer marked failed |
| `TestUploadCancellation` / `TestDownloadCancellation` | context cancel mid-transfer → cancelled state |
| `TestDownloadOffsetAlignment` | unaligned offsets byte-exact; windowed truncation |
| `TestDownloadSizeIntegrity` | declared size > served bytes → integrity failure |
| `TestConcurrentTransfersBounded` | semaphore admits exactly N; queued rest; exact metrics |
| `TestCloseDuringUpload` / `TestCancelAllTransfersOnClose` | shutdown settles all transfers, no goroutine leak |
| `TestSendMediaRoundTrip` | Upload → sendMedia → Updates → DownloadMedia byte-exact loop |
| `TestProgressPerPart` / `TestProgressPanicIsolated` | callback contract |
| `TestStress100ConcurrentTransfers` | 100 mixed transfers, exact reconciliation |
| `TestStress1000SequentialTransfers` | 1000 sequential, flat heap (240 KB growth) |
| `TestMemoryBoundedLargeUpload` / `TestMemoryBoundedLargeDownload` | 64 MiB logical transfer, ≤ 0.3 MiB live heap growth |
| `TestStressConcurrentMixedWithFailures` | failures racing successes: exact terminal accounting, no stuck slots |
| `FuzzResolveUploadPartSize` / `FuzzResolveDownloadChunkSize` | part-size invariants for any int32 KB |
| `FuzzChunkClassification` | hostile chunk answers never panic the decision table |

## Real-server validation status

**No real Soroush Plus server was used in this phase.** All validation ran
against the in-process fake transport (`transport/wstest`) exercising the
real protocol stack (framing, encryption, TL encoding, RPC engine, media
engines). Access hashes, file IDs, references, and server behaviors in
tests are synthetic test doubles, clearly labeled as such. Upload and
download against a live Soroush Plus instance remains **unverified** and
must not be claimed as otherwise.
