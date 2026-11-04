# Media Domain (Phase 13)

Rich media as a first-class domain concept on top of the Phase 8
transfer engine. Every behavior is traced to the Python reference
(`reference/SPlusthon`, read-only) or the frozen TL schema (layer 182).

**Real Soroush Plus validation: NOT PERFORMED — no real credentials
available.** All evidence comes from the in-process fake transport
exercising the real protocol stack plus reference-executed golden data.

## Canonical conversion path (§8/§18)

```
TL Message ──► messageFromTL ──► classifyMedia ──► Message.Media (domain)
                                      │
        history / search / dialogs / mutation responses / updates
        ALL route through messageFromTL — no duplicated media parsing.
```

The classifier (`classifyMedia`, `client/media_domain.go`) handles every
`MessageMedia*` family in layer 182. Unknown families become
`UnknownMedia{KindName, Detail}` — never a panic, never fabricated
content; a `nil` wire media produces a `nil` domain Media.

Reference trace:

| Behavior | Reference | Lines | Go | Tests |
|---|---|---|---|---|
| photo/document accessors | tl/custom/message.py | 543-573 | classifyMedia | TestClassifierFamilies |
| video/audio/voice/sticker/animated kind | message.py | 585-631, 1237-1247 | Document.MediaKind | TestDocumentAttributeClassification |
| photo size sort/select | client/downloads.py | 783-825 | Photo.sortedSizes/Largest/Smallest/BestSize | TestPhotoSizeSelection, FuzzPhotoSizeSelection |
| stripped→JPG splice | utils.py | 1530-1544 | strippedPhotoToJPG (+ generated header) | TestStrippedToJPGGolden, FuzzStrippedPhoto |
| stripped byte count | utils.py | 1547-1560 | photoStrippedByteCount | TestPhotoSizeSelection |
| download filenames/kind | client/downloads.py | 916-935 | Document.DisplayName, mediaKindNames | TestAttributeConversion |
| photo download location | client/downloads.py | 848-880 | resolvePhotoLocation | TestLocationResolution, TestDownloadPhotoStrippedE2E |
| document download location | client/downloads.py | 911-951 | resolveDocumentLocation | TestDownloadDocumentE2E |
| file-reference refresh | client/downloads.py | 120-148 | refreshMediaReference | (refresh path, once) |
| grouped_id | tl/custom/message.py | 152-154, 267 | Message.GroupedID/InAlbum, AlbumCollector | TestAlbumCollector* |
| sendMultiMedia | tl/schema/api.tl | messages.sendMultiMedia#456e8987 | SendAlbum | TestSendAlbumE2E |
| existing-media resend | uploads.py (InputMediaPhoto/Document) | — | mediaInputToWire | TestSendMediaExistingDocumentResend |

## Domain model (§5)

```
Media (interface: MediaKind() MediaKind)
├── *Photo         {ID, AccessHash, FileReference, Date, Sizes, VideoSizes, HasStickers}
├── *Document      {ID, AccessHash, FileReference, Date, MimeType, Size, Attributes, Thumbs, VideoThumbs}
│     ├── MediaKind() ∈ {Video, Audio, Voice, Sticker, Animated, Document}
│     └── Attributes []DocumentAttribute (Filename/Video/Audio/Sticker/
│                                        Animated/HasStickers/Unknown)
└── *UnknownMedia  {KindName, Detail}
```

Plain data only: no transport references, no client mutexes, safe to
retain after RPC completion and to pass between goroutines. Kind comes
from ATTRIBUTES, never the MIME type (message.py:585-631 semantics; a
`video/mp4` document without a video attribute stays `Document` —
`TestDocumentAttributeClassification` "mime lies" case).

Unknown wire attributes are preserved as `UnknownAttribute{WireCtor}` —
never dropped, never guessed.

## Photo size selection (§9)

`Photo.sortedSizes()` applies the reference's sort (downloads.py:790-810):
photo sizes by byte count, video sizes after photo sizes at equal
length, Empty last. Selection APIs:

- `Largest()` — the last usable size (downloads.py:822 `thumbs[-1]`)
- `Smallest()` — the first usable size
- `BestSize(maxBytes)` — the LARGEST size fitting the budget; when
  nothing fits, the smallest usable size (documented graceful
  degradation; deterministic under fuzz)

Cached/Stripped sizes are inline payloads: the download returns their
bytes (spliced to JPEG for stripped) without any RPC
(`_download_cached_photo_size`, downloads.py:826-845).

## FileReference lifecycle (§6)

- **Obtained**: only from server responses (message envelopes, dialog
  pages) — copied verbatim into the domain types.
- **Stored**: inside `Photo.FileReference` / `Document.FileReference`
  (the caller's Message retains them; no separate media cache — §19).
- **Used**: location resolution passes the bytes through untouched
  (downloads.py:874/939); an EMPTY reference passes through too (the
  server's choice — never fabricated into a non-empty one).
- **Refreshed**: on `FILE_REFERENCE_EXPIRED` during download, the
  message is re-fetched by ID; the fresh reference is adopted ONLY when
  the media id+hash are unchanged (downloads.py:125-147 — a different
  media means the content was edited and the original error stands).
  Exactly one retry.
- **Invalidated**: never silently reused across different media; the
  identity check (kind + id + access hash, excluding the rotating
  reference) guards the swap.

## Media location resolution (§16)

One resolver, `resolveMediaLocation(Media)`:

| Media | Location | Size |
|---|---|---|
| *Photo (regular size) | `InputPhotoFileLocation{id, hash, ref, thumb_size: size letter}` | size's byte count |
| *Photo (cached/stripped) | none — inline bytes | — |
| *Document (full) | `InputDocumentFileLocation{..., thumb_size: ""}` | document.size |
| *Document (thumb) | `InputDocumentFileLocation{..., thumb_size: letter}` | thumb byte count |
| anything else | `ErrUnsupportedMedia` | — |

Missing access hash or unusable size → `ErrMediaLocation` (structural,
§22). No malformed wire object is ever built.

## Sending (§13/§14)

```
UploadedPhotoInput / UploadedDocumentInput  → InputMediaUploaded*  (new bytes; Phase 8 engine)
ExistingPhotoInput  / ExistingDocumentInput → InputMediaPhoto/Document (server addressing, NO re-upload)
SendAlbum(peer, []MediaInput, SendAlbumOptions)             → messages.sendMultiMedia
```

- Existing-media resend never touches the upload engine (verified: zero
  saveFilePart RPCs, `TestSendMediaExistingDocumentResend`).
- `SendAlbum` bounds albums at 10 items (defensive; official-client
  norm), per-item captions ride `InputSingleMedia`, per-item crypto
  random ids pair results, `Messageparse` nil-hole semantics preserved.
- Phase 8's `SendMedia(ctx, peer, io.Reader, opts)` signature is
  retained, implemented on top of `SendMediaInput`.

## Albums (§12)

`AlbumCollector` (docs/albums.md): caller-owned, delay-based grouping by
the server's `grouped_id` (events/album.py:19 `_HACK_DELAY=0.2s`,
extended per arrival). Bounded (`MaxPending`, `MaxPerAlbum`; overflow
flushes the oldest album — never a silent drop), no goroutine owned
(deadlines evaluated in `Next` against an injectable clock),
cancellation-safe.

## Update integration (§17)

Own media mutations stay state-only (Phase 10 `OwnUpdate`): zero
subscriber events (`TestStress32ConcurrentMediaSends`). Server-pushed
media updates flow through `messageFromTL` → `classifyMedia` like every
other path (`TestEntityRotationDuringMediaOp`).

## Memory (§21)

- Upload/download: unchanged Phase 8 O(part_size)/O(chunk_size).
- Conversion: metadata only — 1000 media messages add ~0.0 MiB
  (`TestMemory1000MediaMessages`).
- Albums: bounded by MaxPending × MaxPerAlbum.

## Deviations

| Reference | Go | Reason |
|---|---|---|
| `_get_thumb` supports index/type/instance selectors | Go exposes Largest/Smallest/BestSize | typed API; index/letter selection maps to iterating `Sizes` |
| `_download_document` filename inference | `Document.DisplayName()` helper (caller names files) | Go streaming API takes io.Writer; name helpers provided |
| `aiohttp`-based `_download_web_document` | web documents → UnknownMedia | no third-party HTTP dependency (Phase 1 rule) |
| `comment_to`/send-as/multi-album captions | not exposed | scope §2 |
