# Cancellation Contract (Phase 17)

Rules for `context.Context` handling across every network-facing public
API. Every rule below is enforced by a named test.

## Rules

1. **Context is always the first parameter** of every network-facing
   method (`Connect`, `Disconnect`, `SendMessage`, `GetDialogs`,
   `Upload`, `Download`, auth calls, ...). The two exceptions are
   deliberate:
   - `Close() error` — the terminal barrier must not be cancellable
     (standard Go convention; termination is itself the cancellation of
     everything else).
   - `CatchUp() error` — it posts a catch-up request to the client's
     owning sync loop and returns that loop's result; there is no caller
     deadline to splice. Close the client to interrupt it.
2. **Cancellation before invocation** returns the context error
   immediately, without any network I/O
   (`TestContextCancellationWins`, lifecycle_test.go).
3. **Cancellation during network wait** unblocks the caller with
   `context.Canceled`/`context.DeadlineExceeded`; the pending RPC is
   finalized with the context error
   (`TestShortDeadlineCancellation`, hardening_test.go).
4. **Deadlines are never swallowed.** Deadline expiry is returned exactly,
   even when retries are configured — retry backoff observes ctx.Done()
   (`invokeWithRetries`/`retryAfter`, invoke.go:88-131).
5. **Close interrupts operations.** A `Close` during an in-flight invoke,
   retry backoff, update delivery, or subscriber churn fails the affected
   operations deterministically with `ErrClosed`
   (hardening_test.go: TestCloseDuringInvoke, TestCloseDuringRetryBackoff,
   TestCloseDuringUpdateDelivery, TestCloseDuringSubscriptionChurn).
   Precedence: a *caller* context that is already done wins over ErrClosed;
   otherwise ErrClosed wins over the spliced lifecycle error
   (docs/error-contract.md §Precedence).
6. **Cancellation during retry** stops the backoff timer and returns the
   context error; no further attempts are made (invoke.go retryAfter
   select on ctx.Done; exercised by TestCloseDuringRetryBackoff and
   TestContextCancellationWins).
7. **Cancellation during pagination** terminates the iterator; `Next()`
   returns false and `Err()` reports the context error
   (TestDialogsContextCancel, TestSearchContextCancel; sticky-error rule
   in docs/error-contract.md).
8. **Cancellation during media transfer** stops the transfer at the
   current part/chunk boundary with a context error and a terminal CAS'd
   transfer state (TestUploadCancellation, TestDownloadCancellation,
   TestCancelAllTransfersOnClose — Close cancels every active transfer).
9. **No background goroutine survives cancellation/Close.** The client's
   fixed goroutine set (sync loop, workers, transfer pool) is bounded and
   joined by the Close barrier; 100-cycle leak tests (runtime_test.go)
   verify the goroutine count returns to baseline.
10. **Cancellation is preserved, not replaced.** Where the client splices
    the caller context with its lifecycle context (mergeContext), the
    caller's cause is returned when the caller fired first; errors are
    unwrapped to the exact `context.Canceled`/`DeadlineExceeded` values so
    `errors.Is` matches.

## Caller responsibility

- Pass a context to every network call; use `context.WithTimeout` for any
  operation that must not hang on a sick server.
- Do not reuse a cancelled context for subsequent calls.
- Treat `context.Canceled` as "you asked to stop", `DeadlineExceeded` as
  "the operation did not finish in time" — neither indicates a protocol
  failure, and both are safe to retry with a fresh context *except* for
  non-idempotent mutations (docs/retry-policy.md).

## Test index

| Scenario | Test |
|---|---|
| Cancellation before invocation | TestContextCancellationWins |
| Deadline expiration | TestShortDeadlineCancellation |
| Close during operation | TestCloseDuringInvoke, TestCloseDuringConnect |
| Cancellation during retry backoff | TestCloseDuringRetryBackoff |
| Cancellation during pagination | TestDialogsContextCancel, TestSearchContextCancel |
| Cancellation during media transfer | TestUploadCancellation, TestDownloadCancellation, TestCancelAllTransfersOnClose |
| Cancellation during auth | TestAuthCancellation |
| Connect cancellation | TestConnectCancellation |
| Transport-level send cancellation | tests/transport: TestContextCancellationDuringSend |
| Mutation cancellation | TestMutationCancellation |
| No goroutine leaks after cancellation storms | runtime_test.go leak suite |
