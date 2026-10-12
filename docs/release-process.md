# Release Process (Phase 19)

A deterministic, gate-driven procedure. **Every gate is a command with a
binary outcome; none can be waived.** The live gate (step 13) is
mandatory: no release may bypass Phase 18's verdict.

## 0. Preconditions (owner decisions that gate the process)

- Distribution license selected and `LICENSE` added
  (docs/licensing.md — currently UNRESOLVED).
- Module path finalized (`suromesh` is placeholder-grade;
  docs/versioning.md documents the migration procedure).
- Source tree committed to VCS and tagged (the project root currently
  has no VCS — docs/reproducibility.md).

## 1. Clean checkout

```sh
git clone <repo> /work/checkout && cd /work/checkout
git checkout <tag-candidate>        # or the release commit
git status --porcelain              # MUST be empty
```

## 2. Environment verification

```sh
go version                          # ≥ go.mod's declared version (1.26)
go env CGO_ENABLED                  # any; source is cgo-free (verified)
```

## 3. Generator verification

```sh
go run ./cmd/tlgen -verify
# expected: 47 files (1684 constructors, LAYER 182), output is current
# any diff ⇒ STOP (nondeterminism or drift — docs/reproducibility.md)
```

## 4. Dependency verification

```sh
go mod tidy && git diff --exit-code go.mod go.sum   # no-op required
go mod verify                                        # all modules verified
```

## 5–7. Test / race / static gates

```sh
go build ./...
go vet ./...
gofmt -l .                                   # zero files
go test -count=1 ./...                       # all packages ok
go test -race -count=1 ./...                 # × 3 consecutive runs
staticcheck ./...                            # exactly one finding allowed:
                                             # tl/gzip.go:38 (frozen, documented)
go test -count=1 -run Fuzz ./...             # all seed corpora
```

## 8. Secret audit

```sh
# no credential-shaped strings outside reference/ and known fixtures:
grep -rniE "begin .*private key|bearer |password\s*=\s*['\"][^'\"]{6,}" \
     --include="*.go" --include="*.md" . | grep -v ./reference/
find . -name "*.session" -o -name "*.env" -o -name "*.pem" -o -name "*.key" \
     | grep -v ./reference/                  # must be empty
# triage against docs/security.md classifications; ANY unexplained hit ⇒ STOP
```

## 9. License audit

```sh
test -f LICENSE || echo "RELEASE BLOCKER — LICENSE UNRESOLVED"
grep -c "GPL" docs/licensing.md              # facts must still hold
```

## 10. Documentation audit

Cross-check the claims listed in Phase 19 §10 (layer, constructor count,
live-validation status, dependency status, versioning). The README must
still state that real Soroush Plus validation has NOT been performed.

## 11. Version / tag preparation

- `v0.y.z` per docs/versioning.md rules (additive = minor; fixes =
  patch). v1.0 is NOT allowed until step 13 passes.
- Tag the exact commit the gates ran on.

## 12. Artifact verification

```sh
go build ./... && go vet ./...               # on the tagged tree
# source distribution excludes (docs/release-artifacts.md):
#   reference/**, test logs, coverage, session files
# consumer simulation on the tagged tree:
#   fresh module + replace ⇒ import client/session/tl/gen ⇒ build+run
#   (the Phase 19 §8 probe; internal imports must fail to compile)
```

## 13. Live validation gate — MANDATORY

```sh
# per tests/live/README.md runbook; operator-supplied credentials only
SP_SDK_LIVE=1 SP_LIVE_AREAS=... go test ./tests/live/ -v
```

The release is **RELEASE QUALIFIED** only when every critical live area
(auth, session reuse, messaging, updates, reconnect, media upload/
download, error handling) reports PASS with sanitized evidence, per
Phase 18 §33. Until then the standing verdict is:

```text
RELEASE BLOCKED — LIVE CREDENTIALS REQUIRED
```

## 14. Final release decision

Combine: steps 1–12 all green + license resolved + **step 13 PASSED** ⇒
`RELEASE QUALIFIED` (and only then, `V1.0 ELIGIBLE`). Steps 1–12 green
without step 13 ⇒ `ENGINEERING READY` (the Phase 19 outcome).
