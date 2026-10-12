# Drafts (Phase 14) — DEFERRED

**Status: DEFERRED.**

Reason: the layer-182 schema (`tl/schema/api.tl`) contains
`updateDraftMessage#1b49ec6d` (the update constructor — it flows through
the MessageBox to subscribers raw) but does **not** declare the draft
RPCs the reference uses:

- `messages.getAllDrafts`
- `messages.getPeerDrafts`
- `messages.setDraftMessage`
- `messages.clearAllDrafts`

(reference `iter_drafts`, dialogs.py:267-324, traces against these
absent constructors.)

Implementing draft get/set would require modifying the frozen
schema/generator — prohibited without a genuine correctness defect
(Phase 14 §21). A client-layer workaround cannot exist: there is no
constructor to encode.

Prerequisite for a future phase: the schema gains the draft RPC family
→ regenerate → `GetDraft/SetDraft/ClearDraft/ListDrafts` implemented on
the peer-scoped model with `updateDraftMessage` driving derived state.

What exists today: `updateDraftMessage` updates already flow through the
MessageBox to subscribers (raw update family, no special handling
required); the Phase 8 `SendMessageOptions.ClearDraft` flag sends
`clear_draft` on sends, per the reference.
