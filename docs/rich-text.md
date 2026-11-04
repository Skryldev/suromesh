# Rich Text — Message Entities (Phase 14)

The typed entity layer for message text.

Reference:
- `extensions/markdown.py:62` — "work on byte level with the utf-16le
  encoding to get the offsets right"
- `helpers.py:39-45` — `add_surrogate`: "SoroushPlus offsets are
  calculated with these [surrogate pairs]"
- layer-182 `MessageEntity*` constructors (all 18 generated)

## UTF-16 offset semantics (§5)

Entity `Offset` and `Length` are **UTF-16 code units** on the message
text — NOT Go bytes and NOT runes. Consequences:

- an astral-plane character (emoji, some CJK) is TWO units;
- BMP characters (Latin, Persian, Cyrillic) are ONE unit;
- Go strings are UTF-8, so `utf16Len`/`utf16Slice` are provided for
  callers converting between representations.

`TestEntityOffsetsDifferential` pins the rule against a
Persian/English/emoji mixed message: "سلام 😀 world" is 13 units
(5 + 2 + 6), the emoji spans units 5–6, and slicing by UTF-16 offsets
round-trips exactly. Astral-plane, mixed-script and out-of-range cases
are covered; malformed entities clamp (never panic).

## Supported entities

| Kind | Wire ctor | Payload |
|---|---|---|
| Mention, Hashtag, BotCommand, URL, Email, Phone, Cashtag | messageEntity* | span only |
| Bold, Italic, Underline, Strike, Spoiler, Blockquote, Code | messageEntity* | span only |
| Pre | messageEntityPre#74ae209d | Language |
| TextURL | messageEntityTextUrl#76a6dAA6 | URL |
| MentionName | messageEntityMentionName#dc7b1140 | UserID |
| CustomEmoji | messageEntityCustomEmoji#c927c44c | DocumentID |
| unknown | any other ctor | UnknownTextEntity{WireCtor, Raw} — preserved |

All 18 generated constructors convert; unknown constructors are
preserved structurally (`UnknownTextEntity`) and surfaced through
`Message.UnknownTextEntities` (§13 — forward compatibility, no panic,
no silent drop).

## Conversion

`textEntitiesFromWire` runs inside `messageFromTL` — the single
canonical message path — so every source (history, search, updates,
mutations, dialogs) produces identical `Message.TextEntities`.

## Public shape

```go
type TextEntity struct {
    Kind       TextEntityKind
    Offset     int32  // UTF-16 code units
    Length     int32  // UTF-16 code units
    URL        string // TextURL
    Language   string // Pre
    UserID     int64  // MentionName
    DocumentID int64  // CustomEmoji
}
```

Raw wire entities remain reachable via `Message.Entities`
(deprecated for reading) and `Message.Raw`.

## Malformed input

Out-of-range or negative offsets never panic: `utf16Slice` clamps or
rejects (`TestEntityOffsetsDifferential` negative-offset case);
`FuzzMessageEntities` pins the conversion against arbitrary wire
constructors.
