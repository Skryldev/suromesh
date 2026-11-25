# SDK↔Service Conformance Reconciliation (Phase 27)

## What was wrong in the Phase 26 report

The Phase 26 report stated "33 rows" but accounted for only **28**
classifications (15 FAITHFUL + 9 FAITHFUL_WITH_DOCUMENTED_ADAPTATION +
1 PARTIALLY_FAITHFUL + 1 NOT_EXPOSED + 2 UNSUPPORTED). Re-verification of
the actual matrix (`sdk-conformance-matrix.csv`) shows the correct totals
are:

| Classification                          | Phase 26 report | Actual (verified Phase 27) |
|------------------------------------------|-----------------|-----------------------------|
| FAITHFUL                                 | 15              | **19** |
| FAITHFUL_WITH_DOCUMENTED_ADAPTATION      | 9               | **10** |
| PARTIALLY_FAITHFUL                       | 1               | **1** |
| NOT_EXPOSED                              | 1               | **1** |
| UNSUPPORTED                              | 2               | **2** |
| SEMANTIC_MISMATCH                        | 0               | **0** |
| **Total rows**                           | 28 (inconsistent with "33 rows") | **33** |

The matrix data itself was correct; the report's arithmetic was not. The
28-vs-33 discrepancy came from dropping 5 rows when summarizing (R07
Connect/Disconnect split, R26 raw Invoke, and the peer-info grouping rows
were counted as one).

## Method-level accounting (73 methods)

The SDK exposes **exactly 73 exported methods** on `*Client` in non-test
source files (verified by extraction, `tools/apidocs/gen_method_inventory.py`).
Every method is accounted for **exactly once** in
`sdk-method-inventory.csv`:

| Method-level class  | Count | Meaning |
|---------------------|-------|---------|
| EXPOSED             | 33    | adapter calls the method; capability reaches the service API via its matrix row |
| GROUPED_EXPOSED     | 12    | capability reaches the API through a sibling/variant method (e.g. `SendMessage` via `SendMessageOpts`); grouping documented per row |
| ADAPTER_INTERNAL    | 4     | used inside the adapter/SDK machinery; raw capability deliberately unexposed (`Invoke` — ADR-0003) |
| SDK_SUPPORT         | 15    | diagnostics/tuning/caches; deliberately not exposed |
| TEST_SUPPORT        | 9     | exported test helpers (`*ForTest`, `Seed*`); never load-bearing for production |
| **Total**           | **73**| verified == 73 exported methods |

## Row ↔ method grouping (why 33 rows ≠ 73 methods)

The matrix is keyed by **service capability**, not by SDK method. Justified
groupings:

- **R10** GetUser/Channel/Chat — groups 6 SDK methods
  (`GetUser`, `GetUserFull`, `GetChannel`, `GetChannelFull`, `GetChat`,
  `GetChatFull`).
- **R18** Pin/Unpin — groups 2 methods.
- **R13** SendMessage — groups `SendMessageOpts` (adapter entry) +
  `SendMessage`/`Reply` as grouped variants.
- **R12** ListDialogs — groups `GetDialogs` (adapter entry) +
  `IterDialogs` (iterator variant).
- **R14** GetMessages — implemented on the SDK public `Invoke` path
  (documented adaptation; `GetMessages` grouped).
- **R33** UNSUPPORTED families — groups `SendAlbum`, `SearchGlobal`,
  `IterGlobalSearch`, `ResolveReply` (SDK implements them; service v1
  scope does not).
- **R09** ResolvePeer family — groups `ResolvePeer` (adapter entry),
  `ResolvePeerString`, `ResolveUsername`, `InputPeerFor`, `InputPeerSelf`,
  `AuthState`.
- **R07** Connect/Disconnect/GetConnectionState — groups `Connect`,
  `Disconnect`, `Close` (runner-owned lifecycle), `IsConnected`.
- **R24** UploadMedia — groups `SendMedia`, `SendMediaInput`, `Upload`.
- **R25** DownloadMedia — groups `DownloadMedia` (adapter entry) and
  `Download` (SDK-internal raw location download).

All other rows are 1:1 with their adapter entry method. Full per-method
traceability (method → source → classification → matrix row → reason)
lives in `sdk-method-inventory.csv`.

## Denominators used in reporting

- Capability-level percentages use **33 matrix rows** as denominator.
- Method-level percentages use **73 exported methods** as denominator.
- The two must never be mixed: "service covers X% of SDK surface" is a
  method-level statement (73), while "of the audited capability rows, Y%
  are FAITHFUL" is a row-level statement (33).
