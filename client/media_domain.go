package client

// Media domain model (Phase 13). The canonical Message → Media classifier
// and the domain types, ported from the reference:
//
//	Message.photo / .document / .video / .audio / .voice / .sticker
//	  reference/SPlusthon/splusthon/tl/custom/message.py:524-631
//	_document_by_attribute       message.py:1237-1247
//	_get_thumb (size selection)  client/downloads.py:783-825
//	stripped_photo_to_jpg        utils.py:1530-1544
//	_photo_size_byte_count       utils.py:1547-1560
//	_get_kind_and_names          client/downloads.py:916-935
//
// Design rules (§5/§8/§22):
//   - domain values are immutable-by-convention; safe to retain after the
//     RPC completes and to pass between goroutines (plain data only)
//   - ONE canonical classifier: every Message → Media conversion goes
//     through classifyMedia — history, search, dialogs, mutation
//     responses and updates share it (no duplicated parsing)
//   - access hashes and file references are preserved verbatim from the
//     server; unknown media becomes UnknownMedia (never a panic)

import (
	"errors"
	"fmt"
	"time"

	"github.com/Skryldev/suromesh/tl/gen"
)

// MediaKind discriminates the supported media families.
type MediaKind int32

const (
	MediaKindUnknown MediaKind = iota
	MediaKindPhoto
	MediaKindVideo
	MediaKindAudio
	MediaKindVoice
	MediaKindSticker
	MediaKindAnimated
	MediaKindDocument
)

func (k MediaKind) String() string {
	switch k {
	case MediaKindPhoto:
		return "photo"
	case MediaKindVideo:
		return "video"
	case MediaKindAudio:
		return "audio"
	case MediaKindVoice:
		return "voice"
	case MediaKindSticker:
		return "sticker"
	case MediaKindAnimated:
		return "animated"
	case MediaKindDocument:
		return "document"
	}
	return "unknown"
}

// Media is the domain interface for message media. Plain data only: no
// transport references, no client state, safe across goroutines.
type Media interface {
	MediaKind() MediaKind
}

// UnknownMedia carries media the library recognizes but does not model
// (contacts, polls, geo, web previews' inner media, empty placeholders).
// It is never a panic and never fabricated content.
type UnknownMedia struct {
	KindName string // wire constructor family, e.g. "MessageMediaPoll"
	Detail   string // why it is unsupported ("empty photo", ...)
}

func (u *UnknownMedia) MediaKind() MediaKind { return MediaKindUnknown }

// Photo is a server-provided photo with its size variants.
type Photo struct {
	ID            int64
	AccessHash    int64
	FileReference []byte
	Date          time.Time
	// Sizes holds the photo's size variants (normalized, selectable).
	Sizes []PhotoSize
	// VideoSizes holds video-size variants (preview clips; layer 182).
	VideoSizes  []VideoSizeInfo
	HasStickers bool
}

func (p *Photo) MediaKind() MediaKind { return MediaKindPhoto }

// PhotoSizeKind discriminates the wire size variants.
type PhotoSizeKind int32

const (
	PhotoSizeEmptyK PhotoSizeKind = iota
	PhotoSizeExact
	PhotoSizeProgressive
	PhotoSizeCached
	PhotoSizeStripped
	PhotoSizeVideo
)

// PhotoSize is the normalized domain form of the five photo-size
// constructors (PhotoSize, PhotoSizeProgressive, PhotoCachedSize,
// PhotoStrippedSize, PhotoSizeEmpty) plus VideoSize.
type PhotoSize struct {
	Kind PhotoSizeKind
	Type string // wire thumb-size letter ("", "s", "m", "x", "y", "w", …)
	W, H int32

	// Bytes carries Cached/Stripped inline payloads (download-free).
	Bytes []byte
	// Sizes carries Progressive's byte-count ladder; ByteCount = max.
	Progressive []int32
	// ByteCount is the reference's _photo_size_byte_count (utils.py:1547).
	ByteCount int64
}

// VideoSizeInfo is the domain form of VideoSize (preview clips).
type VideoSizeInfo struct {
	Type     string
	W, H     int32
	Size     int32
	Duration float64
}

// mediaSizeSortKey mirrors downloads.py:790-803 — the sort key tuple
// (primary, secondary) with Empty/invalid LAST (0,0) and video sizes
// after photo sizes at equal lengths.
func mediaSizeSortKey(p PhotoSize) (int, int64) {
	switch p.Kind {
	case PhotoSizeStripped, PhotoSizeCached:
		return 1, int64(len(p.Bytes))
	case PhotoSizeExact:
		return 1, int64(p.ByteCount)
	case PhotoSizeProgressive:
		return 1, int64(p.ByteCount) // max(Sizes), precomputed
	case PhotoSizeVideo:
		return 2, int64(p.ByteCount)
	default:
		return 0, 0 // PhotoSizeEmptyK / unknown: last (downloads.py:802-803)
	}
}

// sortedSizes returns the size list in the reference's ascending order
// (downloads.py:803-810): photo sizes by byte count, video sizes after
// photo sizes at equal length, Empty/invalid last.
func (p *Photo) sortedSizes() []PhotoSize {
	all := make([]PhotoSize, 0, len(p.Sizes)+len(p.VideoSizes))
	all = append(all, p.Sizes...)
	for _, v := range p.VideoSizes {
		all = append(all, PhotoSize{Kind: PhotoSizeVideo, Type: v.Type,
			W: v.W, H: v.H, ByteCount: int64(v.Size)})
	}
	// Stable insertion sort by (primary, secondary) — batches are tiny.
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && mediaSizeLess(all[j], all[j-1]); j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	return all
}

// mediaSizeLess orders two normalized sizes by the reference's key.
func mediaSizeLess(a, b PhotoSize) bool {
	pa, sa := mediaSizeSortKey(a)
	pb, sb := mediaSizeSortKey(b)
	if pa != pb {
		return pa < pb
	}
	return sa < sb
}

// Largest returns the biggest usable size (downloads.py:822 thumbs[-1]).
func (p *Photo) Largest() (PhotoSize, bool) {
	s := p.sortedSizes()
	for i := len(s) - 1; i >= 0; i-- {
		if s[i].Kind != PhotoSizeEmptyK {
			return s[i], true
		}
	}
	return PhotoSize{}, false
}

// Smallest returns the smallest usable size.
func (p *Photo) Smallest() (PhotoSize, bool) {
	for _, s := range p.sortedSizes() {
		if s.Kind != PhotoSizeEmptyK {
			return s, true
		}
	}
	return PhotoSize{}, false
}

// BestSize returns the LARGEST size whose byte count ≤ maxBytes
// (deterministic graceful degradation: when nothing fits, the smallest
// usable size is returned rather than nothing).
func (p *Photo) BestSize(maxBytes int64) (PhotoSize, bool) {
	s := p.sortedSizes()
	var largest *PhotoSize
	for i := range s {
		if s[i].Kind == PhotoSizeEmptyK {
			continue
		}
		if largest == nil || s[i].ByteCount > largest.ByteCount {
			largest = &s[i]
		}
	}
	var best *PhotoSize
	for i := range s {
		if s[i].Kind == PhotoSizeEmptyK {
			continue
		}
		if s[i].ByteCount <= maxBytes {
			if best == nil || s[i].ByteCount > best.ByteCount {
				best = &s[i]
			}
		}
	}
	if best != nil {
		return *best, true
	}
	if largest != nil {
		return *largest, true
	}
	return PhotoSize{}, false
}

// Document is a server-provided document (files, video, audio, voice,
// stickers, animated media — discriminated by attributes).
type Document struct {
	ID            int64
	AccessHash    int64
	FileReference []byte
	Date          time.Time
	MimeType      string
	Size          int64
	Attributes    []DocumentAttribute
	Thumbs        []PhotoSize
	VideoThumbs   []VideoSizeInfo
}

func (d *Document) MediaKind() MediaKind {
	// Reference _document_by_attribute ordering (message.py:585-631):
	// the kind comes from attributes, never the MIME type.
	if d.HasAttribute(attrSticker) {
		return MediaKindSticker
	}
	if a := d.audioAttr(); a != nil {
		if a.Voice {
			return MediaKindVoice
		}
		return MediaKindAudio
	}
	if d.HasAttribute(attrVideo) {
		return MediaKindVideo
	}
	if d.HasAttribute(attrAnimated) {
		return MediaKindAnimated
	}
	return MediaKindDocument
}

// DocumentAttributeKind discriminates attribute implementations.
type DocumentAttributeKind int32

const (
	AttributeUnknown DocumentAttributeKind = iota
	AttributeFilename
	AttributeVideo
	AttributeAudio
	AttributeSticker
	AttributeAnimated
	AttributeHasStickers
)

const (
	attrFilename  DocumentAttributeKind = AttributeFilename
	attrVideo     DocumentAttributeKind = AttributeVideo
	attrAudio     DocumentAttributeKind = AttributeAudio
	attrSticker   DocumentAttributeKind = AttributeSticker
	attrAnimated  DocumentAttributeKind = AttributeAnimated
	attrHasSticks DocumentAttributeKind = AttributeHasStickers
)

// DocumentAttribute is the type-safe attribute abstraction; unknown
// wire attributes become UnknownAttribute (preserved, never dropped).
type DocumentAttribute interface {
	Kind() DocumentAttributeKind
}

// FilenameAttribute (DocumentAttributeFilename).
type FilenameAttribute struct{ FileName string }

func (a FilenameAttribute) Kind() DocumentAttributeKind { return AttributeFilename }

// VideoAttribute (DocumentAttributeVideo).
type VideoAttribute struct {
	RoundMessage      bool
	SupportsStreaming bool
	Nosound           bool
	Duration          float64
	W, H              int32
}

func (a VideoAttribute) Kind() DocumentAttributeKind { return AttributeVideo }

// AudioAttribute (DocumentAttributeAudio).
type AudioAttribute struct {
	Voice     bool
	Duration  int32
	Title     string
	Performer string
	Waveform  []byte
}

func (a AudioAttribute) Kind() DocumentAttributeKind { return AttributeAudio }

// StickerAttribute (DocumentAttributeSticker).
type StickerAttribute struct {
	Mask       bool
	Alt        string
	Stickerset string // short name of the set (server-provided; "" when masked)
	MaskCoords bool
}

func (a StickerAttribute) Kind() DocumentAttributeKind { return AttributeSticker }

// AnimatedAttribute (DocumentAttributeAnimated).
type AnimatedAttribute struct{}

func (a AnimatedAttribute) Kind() DocumentAttributeKind { return AttributeAnimated }

// HasStickersAttribute (DocumentAttributeHasStickers).
type HasStickersAttribute struct{}

func (a HasStickersAttribute) Kind() DocumentAttributeKind { return AttributeHasStickers }

// UnknownAttribute preserves unrecognized wire attributes (§5: unknown
// constructors must not panic and must not be silently dropped).
type UnknownAttribute struct {
	WireCtor uint32
}

func (a UnknownAttribute) Kind() DocumentAttributeKind { return AttributeUnknown }

// HasAttribute reports whether the document carries an attribute kind.
func (d *Document) HasAttribute(kind DocumentAttributeKind) bool {
	for _, a := range d.Attributes {
		if a.Kind() == kind {
			return true
		}
	}
	return false
}

// Filename returns the wire-provided file name (downloads.py:916-935
// inserts DocumentAttributeFilename names first) — "" when absent.
func (d *Document) Filename() string {
	for _, a := range d.Attributes {
		if f, ok := a.(FilenameAttribute); ok {
			return f.FileName
		}
	}
	return ""
}

// audioAttr returns the DocumentAttributeAudio, if any.
func (d *Document) audioAttr() *AudioAttribute {
	for _, a := range d.Attributes {
		if au, ok := a.(AudioAttribute); ok {
			return &au
		}
	}
	return nil
}

// videoAttr returns the DocumentAttributeVideo, if any.

// IsVideo: has DocumentAttributeVideo (message.py:600-605).
func (d *Document) IsVideo() bool { return d.HasAttribute(attrVideo) }

// IsAudio: DocumentAttributeAudio with voice=false (message.py:585-590).
func (d *Document) IsAudio() bool {
	a := d.audioAttr()
	return a != nil && !a.Voice
}

// IsVoice: DocumentAttributeAudio with voice=true (message.py:592-598).
func (d *Document) IsVoice() bool {
	a := d.audioAttr()
	return a != nil && a.Voice
}

// IsSticker: DocumentAttributeSticker (message.py:627-631).
func (d *Document) IsSticker() bool { return d.HasAttribute(attrSticker) }

// IsAnimated: DocumentAttributeAnimated (message.py:616-624).
func (d *Document) IsAnimated() bool { return d.HasAttribute(attrAnimated) }

// DisplayName mirrors _get_kind_and_names (downloads.py:916-935): the
// best available name for the file — filename first, then audio
// "performer - title" forms.
func (d *Document) DisplayName() string {
	if f := d.Filename(); f != "" {
		return f
	}
	if a := d.audioAttr(); a != nil {
		switch {
		case a.Performer != "" && a.Title != "":
			return a.Performer + " - " + a.Title
		case a.Performer != "":
			return a.Performer
		case a.Title != "":
			return a.Title
		}
	}
	return ""
}

// ---- canonical classifier --------------------------------------------------

// classifyMedia is THE single Message-media conversion path (§8): every
// consumer (history, search, dialogs, mutation responses, updates) goes
// through messageFromTL → classifyMedia.
func classifyMedia(mm gen.MessageMediaClass) Media {
	switch t := mm.(type) {
	case *gen.MessageMediaPhoto:
		switch p := t.Photo.(type) {
		case *gen.Photo:
			return photoFromWire(p)
		case *gen.PhotoEmpty:
			return &UnknownMedia{KindName: "MessageMediaPhoto", Detail: "empty photo"}
		default:
			return &UnknownMedia{KindName: "MessageMediaPhoto", Detail: fmt.Sprintf("photo %T", t.Photo)}
		}
	case *gen.MessageMediaDocument:
		switch d := t.Document.(type) {
		case *gen.Document:
			return documentFromWire(d)
		case *gen.DocumentEmpty:
			return &UnknownMedia{KindName: "MessageMediaDocument", Detail: "empty document"}
		default:
			return &UnknownMedia{KindName: "MessageMediaDocument", Detail: fmt.Sprintf("document %T", d)}
		}
	case *gen.MessageMediaContact:
		return &UnknownMedia{KindName: "MessageMediaContact"}
	case *gen.MessageMediaGeo:
		return &UnknownMedia{KindName: "MessageMediaGeo"}
	case *gen.MessageMediaGeoLive:
		return &UnknownMedia{KindName: "MessageMediaGeoLive"}
	case *gen.MessageMediaPoll:
		return &UnknownMedia{KindName: "MessageMediaPoll"}
	case *gen.MessageMediaDice:
		return &UnknownMedia{KindName: "MessageMediaDice"}
	case *gen.MessageMediaGame:
		return &UnknownMedia{KindName: "MessageMediaGame"}
	case *gen.MessageMediaWebPage:
		return &UnknownMedia{KindName: "MessageMediaWebPage"}
	case *gen.MessageMediaVenue:
		return &UnknownMedia{KindName: "MessageMediaVenue"}
	case nil:
		return nil
	default:
		return &UnknownMedia{KindName: fmt.Sprintf("%T", mm), Detail: "unclassified media family"}
	}
}

// photoFromWire converts gen.Photo → domain Photo.
func photoFromWire(p *gen.Photo) *Photo {
	out := &Photo{
		ID:            p.ID,
		AccessHash:    p.AccessHash,
		FileReference: p.FileReference,
		Date:          time.Unix(int64(p.Date), 0),
		HasStickers:   p.HasStickers,
	}
	for _, sc := range p.Sizes {
		if s := photoSizeFromWire(sc); s != nil {
			out.Sizes = append(out.Sizes, *s)
		}
	}
	for _, vc := range p.VideoSizes {
		v, ok := vc.(*gen.VideoSize)
		if !ok {
			continue
		}
		out.VideoSizes = append(out.VideoSizes, VideoSizeInfo{
			Type: v.Type_, W: v.W, H: v.H, Size: v.Size,
		})
	}
	return out
}

// photoSizeFromWire normalizes the five size constructors.
func photoSizeFromWire(sc gen.PhotoSizeClass) *PhotoSize {
	switch s := sc.(type) {
	case *gen.PhotoSize:
		return &PhotoSize{Kind: PhotoSizeExact, Type: s.Type_, W: s.W, H: s.H, ByteCount: int64(s.Size)}
	case *gen.PhotoSizeProgressive:
		byteCount := int64(0)
		for _, sz := range s.Sizes {
			if int64(sz) > byteCount {
				byteCount = int64(sz)
			}
		}
		return &PhotoSize{Kind: PhotoSizeProgressive, Type: s.Type_, W: s.W, H: s.H,
			Progressive: append([]int32{}, s.Sizes...), ByteCount: byteCount}
	case *gen.PhotoCachedSize:
		return &PhotoSize{Kind: PhotoSizeCached, Type: s.Type_, W: s.W, H: s.H,
			Bytes: append([]byte{}, s.Bytes...), ByteCount: int64(len(s.Bytes))}
	case *gen.PhotoStrippedSize:
		return &PhotoSize{Kind: PhotoSizeStripped, Type: s.Type_,
			Bytes: append([]byte{}, s.Bytes...), ByteCount: photoStrippedByteCount(s.Bytes)}
	case *gen.PhotoSizeEmpty:
		return &PhotoSize{Kind: PhotoSizeEmptyK, Type: s.Type_, ByteCount: 0}
	case *gen.VideoSize:
		return &PhotoSize{Kind: PhotoSizeVideo, Type: s.Type_, W: s.W, H: s.H, ByteCount: int64(s.Size)}
	}
	return nil // unknown size constructor: skipped (never a panic)
}

// photoStrippedByteCount mirrors _photo_size_byte_count (utils.py:1550-1554).
func photoStrippedByteCount(b []byte) int64 {
	if len(b) < 3 || b[0] != 1 {
		return int64(len(b))
	}
	return int64(len(b)) + 622
}

// documentFromWire converts gen.Document → domain Document.
func documentFromWire(d *gen.Document) *Document {
	out := &Document{
		ID:            d.ID,
		AccessHash:    d.AccessHash,
		FileReference: d.FileReference,
		Date:          time.Unix(int64(d.Date), 0),
		MimeType:      d.MimeType,
		Size:          d.Size,
	}
	for _, ac := range d.Attributes {
		out.Attributes = append(out.Attributes, attributeFromWire(ac))
	}
	for _, sc := range d.Thumbs {
		if s := photoSizeFromWire(sc); s != nil {
			out.Thumbs = append(out.Thumbs, *s)
		}
	}
	for _, vc := range d.VideoThumbs {
		v, ok := vc.(*gen.VideoSize)
		if !ok {
			continue
		}
		out.VideoThumbs = append(out.VideoThumbs, VideoSizeInfo{
			Type: v.Type_, W: v.W, H: v.H, Size: v.Size,
		})
	}
	return out
}

// attributeFromWire converts one wire attribute; unknown constructors
// become UnknownAttribute (preserved with their ctor id).
func attributeFromWire(ac gen.DocumentAttributeClass) DocumentAttribute {
	switch a := ac.(type) {
	case *gen.DocumentAttributeFilename:
		return FilenameAttribute{FileName: a.FileName}
	case *gen.DocumentAttributeVideo:
		return VideoAttribute{RoundMessage: a.RoundMessage,
			SupportsStreaming: a.SupportsStreaming, Nosound: a.Nosound,
			Duration: a.Duration, W: a.W, H: a.H}
	case *gen.DocumentAttributeAudio:
		out := AudioAttribute{Voice: a.Voice, Duration: a.Duration,
			Waveform: append([]byte{}, a.Waveform...)}
		if a.Title != nil {
			out.Title = *a.Title
		}
		if a.Performer != nil {
			out.Performer = *a.Performer
		}
		return out
	case *gen.DocumentAttributeSticker:
		out := StickerAttribute{Mask: a.Mask, Alt: a.Alt, MaskCoords: a.MaskCoords != nil}
		if ss, ok := a.Stickerset.(*gen.InputStickerSetShortName); ok {
			out.Stickerset = ss.ShortName
		}
		return out
	case *gen.DocumentAttributeAnimated:
		return AnimatedAttribute{}
	case *gen.DocumentAttributeHasStickers:
		return HasStickersAttribute{}
	default:
		return UnknownAttribute{WireCtor: ac.CtorID()}
	}
}

// mediaKindNames reports the reference's download "kind" for a document
// (downloads.py:916-935): document / audio / voice.

// extensionFromMime is the minimal extension mapping the reference gets
// from utils.get_extension via mimetypes; only common types are mapped
// ("" = unknown — never guessed beyond the stdlib table).

// ErrFileReferenceExpired reports the server's FILE_REFERENCE_EXPIRED:
// the media's file reference must be re-fetched from a fresh message
// (reference downloads.py:120-148 refetches and retries once).
var ErrFileReferenceExpired = errors.New("media: file reference expired (re-fetch the message)")

// ErrMediaLocation reports addressing data that would have to be
// fabricated (missing access hash, file reference or size).
var ErrMediaLocation = errors.New("media: insufficient media addressing data")

// ClassifyMediaForTest exposes the canonical classifier to the external
// test package (production code must use Message.Media).
func (c *Client) ClassifyMediaForTest(mm gen.MessageMediaClass) Media {
	return classifyMedia(mm)
}
