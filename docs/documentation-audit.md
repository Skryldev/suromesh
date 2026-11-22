# Documentation Audit (Phase 23)

Audit behind the Docusaurus website (`website/`): what was verified
against the source, what is documented, what is deliberately absent,
and where the residual gaps are.

## A. Audited public API inventory

Method: `go doc -all ./client` (1,828 lines) + `tools/apidocs`
regeneration; counts re-verified this phase (not inherited from prior
audits).

| Surface | Count | Disposition |
|---|---|---|
| `client.Client` methods | **73** (re-verified) | all groups documented in `docs/operations/*`; full signatures auto-generated (`api/generated/client-methods.md`, 131 headings incl. constructors/type methods) |
| `client` exported types | 86 | key types documented (`api/types`); full inventory remains in `docs/public-api-audit.md` |
| Sentinel errors (client) | 15 + `ErrBadState` | documented in `api/errors` with `Is/As` semantics |
| Typed errors | 9 across packages | documented |
| `session` methods | 32 generated signatures | `api/session` + generated page |
| `tl` / `tl/gen` | composition surface | documented as advanced usage (`api/tl`); `gen` = 1,684 constructors (not itemized by design — schema-derived) |
| `internal/**` | — | **not documented as public API** (toolchain-enforced) |
| Test seams (`SeedRaw`, `SeedEntities`, `ClassifyMediaForTest`, `ResolveMediaLocationForTest`, `ClosedForTest`, `ReconnectTransportForTest`) | 6 | documented as test seams, not consumer API |

## B. Documented feature inventory (coverage matrix)

| Required subject (Phase 23 §4) | Page(s) | Verified against |
|---|---|---|
| A. Getting Started (10 items) | `intro`, `getting-started/*` (8 pages) | module path, `client.New`, examples dir |
| B. Core Concepts (14 items) | `core-concepts/*` (14) | implementation + contracts docs |
| C. Installation & Configuration | `configuration/*` (5) | `client.Config` struct (field-by-field, real defaults) |
| D. Authentication (11 items) | `auth/*` (8) | auth.go, auth state machine, SRP port; **live validation pending — stated on every relevant page** |
| E. Sessions (13 items) | `sessions/*` (7) | session package; string-session compat claim = tested byte parity only |
| F. Client Operations | `operations/*` (10) | 73-method group table; unsupported list explicit |
| G. Messaging Guide | `messaging/*` (8) | signatures verified via go doc this phase |
| H. Updates | `updates/*` (7) | MessageBox port; exactly-once claim = tested property |
| I. Media & Files | `media/*` (7) | upload/download/references/albums; O(part) memory = tested |
| J. Error Handling | `errors/*` (7) | error-contract.md; precedence rules = tested |
| K. Advanced Guides | `advanced/*` (7) | includes honest testing guidance (internal fakes not importable) |
| L. Architecture + diagrams | `architecture/*` (7) — 6 Mermaid diagrams | flowcharts mirror actual pipelines (trace vs reference) |
| M. Examples | `examples/*` (9) + 7 compiled programs in `website/examples/` | compiled + vetted (see D) |
| API Reference | `api/*` (5) + generated (2) | generator = `tools/apidocs`, CI-enforced currency |
| Troubleshooting / FAQ / Contributing | 7 pages | observed failure modes only |

## C. Unsupported features (explicitly documented)

Reactions, participants enumeration/admin ops, folders, invite links,
payments, stories, sponsored content, bot API — scope decisions;
**drafts** — schema-blocked (Layer 182). Documented in
`operations/unsupported` and the capabilities matrix. No page implies
otherwise.

## D. Example validation results

| Program | Compiles | Vets | Runs in CI | Needs credentials at runtime |
|---|---|---|---|---|
| `website/examples/minimal` | ✔ | ✔ | compile only | no |
| `website/examples/first-app` | ✔ | ✔ | compile only | no (connects live when run) |
| `website/examples/auth-flow` | ✔ | ✔ | compile only | **yes** (phone + OTP) |
| `website/examples/session-restore` | ✔ | ✔ | compile only | yes (authorized session) |
| `website/examples/messaging` | ✔ | ✔ | compile only | yes (authorized session) |
| `website/examples/updates` | ✔ | ✔ | compile only | yes |
| `website/examples/media` | ✔ | ✔ | compile only | yes |

CI (`docs.yml`) compiles + vets all of them and regenerates/diffs the
API reference. No example is executed against the live service by CI.
The demo-based programs under the top-level `examples/` (fake server,
fully runnable offline) are linked from the docs.

## E. Incomplete or unverified behaviors (documented uncertainty)

1. **Live-service validation of authenticated flows** — auth, session
   reuse, messaging, media, updates, reconnect, soak: offline evidence
   strong; live pending. Stated on `intro` (Validation status),
   `supported-capabilities`, `auth/overview` (caution box).
2. **Album grouping timing on the live service** — collector semantics
   are reference-defined; timing differences acknowledged in
   `media/albums`.
3. **Basic-chat metadata refresh** — no lightweight RPC in Layer 182
   (`GetChat` cache-only) — documented in `operations/peers`.
4. **First-encrypted-RPC live drop (Phase 21/22)** — classified
   transport/environment; documented in troubleshooting with the
   Phase 22 evidence pointer.

## F. Known documentation gaps

- The generated signature pages intentionally carry **doc-comment
  descriptions**, not prose tutorials; behavioral prose lives in the
  hand-written pages. (Design trade-off: zero signature drift vs. rich
  per-method examples — resolved in favor of drift-safety.)
- `tl/gen` is not itemized (1,684 schema-derived constructors);
  documented as a block with navigation guidance.
- No per-error-code catalog of every possible 400-class server error
  (server-owned surface; classification helpers documented instead).
- Website search index is built at build time (local-search plugin) —
  no incremental search server; acceptable for a site this size.

## G. Licensing concerns relevant to publication

- Website content is original documentation of the MIT-licensed SDK;
  MIT applies (`website/` inherits the repository license).
- No content was copied from `reference/SPlusthon` (absent from the
  tree; never read during this phase).
- The TL schema provenance (verbatim reference data copies) is
  disclosed in `docs/licensing.md` and the contributing page states the
  contribution licensing; the copyright-holder confirmation flagged
  there remains an owner action (LICENSE says `Skryldev`, derived from
  the repository identity).
- No fake deployment URLs are claimed: the config targets
  `https://skryldev.github.io/suromesh/` as the standard project-pages
  path, explicitly documented as the owner's to activate.

## H. Validation summary (this phase)

- `npm ci` install: 1,414 packages, exit 0.
- `npm run build`: **SUCCESS** (broken links = throw; deprecation
  migrated to `markdown.hooks.onBrokenMarkdownLinks`).
- `npm run typecheck` (tsc): clean.
- Broken-link gate: enforced by build (one real catch fixed:
  `/docs/intro` → `/docs/` slug mismatch).
- Go examples: `go build` + `go vet` green (7 programs).
- API reference regenerated; drift check wired into CI.
- Secret scan of `website/`: no phone numbers, OTPs, keys, session
  strings, or reference-derived content (scan below).
- Go SDK regression: suite re-run green post-website (SDK untouched).

## I. Bugs found during this phase

### BUG-23-1 — line-ending normalization broke gofmt and generator verification

- **Symptom**: after the owner's initial commit (made with
  `core.autocrlf=true`), the working tree was checked out with CRLF;
  `gofmt -l` flagged ~all Go files and `tlgen -verify` reported the
  generated tree stale (byte comparison vs CRLF working tree).
- **Root cause**: no `.gitattributes`; Git-for-Windows defaults rewrote
  line endings per checkout — a reproducibility hazard for a project
  whose generator performs byte-exact comparisons.
- **Severity**: high (release reproducibility), fixed same-phase.
- **Fix**: `.gitattributes` (`* text=auto eol=lf`, binary exceptions),
  repo-local `core.autocrlf=false`, working tree converted to LF
  (338 files), `git add --renormalize`.
- **Regression**: `gofmt -l` = 0; `tlgen -verify` green; CI (linux)
  unaffected; future checkouts are LF by attribute rule.
- **Compatibility impact**: none (content unchanged; byte counts
  identical modulo line endings).

### BUG-23-2 — build artifacts not ignored

- **Symptom**: 32k+ staged entries (`website/build/`, `node_modules/`).
- **Root cause**: `.gitignore` (written pre-website) lacked
  `node_modules/`.
- **Fix**: patterns added; trees unstaged; `git check-ignore` verified.
- **Compatibility impact**: none.

### Documentation website environment notes

- The project directory moved from `D:\Golang\splus` to
  `D:\Golang\suromesh` mid-phase (session shell required recovery);
  all paths verified against the new location.
- `reference/SPlusthon` is absent from the moved tree — consistent
  with the Phase 21 exclusion; nothing in the docs depends on it.
