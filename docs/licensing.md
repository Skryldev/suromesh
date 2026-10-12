# Licensing & Attribution

**Decision: MIT** — selected by the repository owner (Phase 21). The
copyright holder line derives from the owner-specified repository
identity (`github.com/Skryldev/suromesh` → `Skryldev`); the owner should
confirm or amend the exact name in `LICENSE` if a different legal entity
is intended.

Distribution of the SuroMesh implementation (all Go source, examples,
docs, CI) is under the MIT license in the project-root `LICENSE`.

## Verified facts

1. **Upstream reference license: GPL-3.0.** `reference/SPlusthon/LICENSE`
   is the GNU General Public License, Version 3 (verified by inspection).
   The reference tree is included in this repository solely as the
   behavioral oracle for differential testing (Phases 1–18) and is frozen
   (0 modified files throughout).

2. **The root `LICENSE` file now exists (MIT, Phase 21).** Historical
   note: the README once claimed "MIT" by assertion while no LICENSE file
   existed and upstream was GPL-3.0; that false claim was corrected in
   Phase 19, and the license was then formally selected by the owner in
   Phase 21.

3. **The bundled TL schema is copied from the reference.**
   `tl/schema/api.tl` and `tl/schema/mtproto.tl` are byte-identical to
   `reference/SPlusthon/splusthon_generator/data/api.tl` /
   `mtproto.tl` (verified with `cmp`). The schema is a functional
   wire-protocol description the generator consumes. Under the owner's
   MIT decision this verbatim inclusion is a recorded, accepted fact of
   provenance — no legal conclusion beyond the owner's decision is
   asserted here, and the fact remains visible in this document for any
   downstream auditor.

4. **The Go source is an original behavioral rewrite.** No Python source
   was copied. The methodology was differential: the reference was
   *executed* (tools/vectors/generate.py, seeded PRNG) to produce golden
   vectors, and its behavior was ported from documented analysis.
   Function-level provenance is cited in comments (e.g.
   `password.py:136-194`) as behavioral references, not copied code.

5. **Generated test vectors are data, produced by executing the
   reference.** `tests/vectors/*.json` (16+ fixture sets) were generated
   by running the GPL reference with a seeded PRNG. Outputs of a program
   are generally not licensed as the program is; they are documented here
   for completeness.

6. **Protocol constants and endpoints are not secrets and not licensed
   content**: constructor IDs, `im-server.splus.ir:443`, and the upstream
   client's built-in default `api_id=1030400` /
   `api_hash=6edb16cf88714a4e9a805e928c39c937` (mirrored from the
   reference client's own public defaults; consumers are expected to
   supply their own — `client.Config.APIID/APIHash`).

## Attribution obligations (once a license is chosen)

- The reference implementation (SPlusthon, GPL-3.0) must be attributed as
  the behavioral specification source wherever the project is published.
- The TL schema must be attributed to the SPlusthon generator data.
- Golden-vector provenance ("executed from the reference") is already
  documented in README, docs/architecture.md, and tests/compatibility.

## Standing separation (unchanged by the MIT decision)

- `reference/SPlusthon` remains **third-party GPL-3.0 material** and
  MUST-NOT-RELEASE (docs/release-artifacts.md). It exists solely as the
  behavioral oracle for differential testing and stays excluded from
  every published artifact.
- The SuroMesh Go implementation remains an original behavioral rewrite;
  no Python source was copied.
- Attribution: the reference implementation is credited as the behavioral
  specification source in the README and docs/reference-compatibility.md;
  the TL schema's provenance is recorded in fact (3).
- If the owner later distributes the full repository source (including
  `reference/`), the GPL-3.0 status of that third-party tree applies to
  it; the recommended release artifact (docs/release-artifacts.md)
  excludes it, keeping the published distribution pure MIT scope.
