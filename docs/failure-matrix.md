# Failure Matrix (Phase 15)

Failure → detection → recovery → state impact → caller-visible error →
retry → metrics → test.

| Failure | Detection | Recovery | State impact | Caller error | Retry | Metric | Test |
|---|---|---|---|---|---|---|---|
| Connection loss | transport run loop | auto-reconnect (budgeted backoff) | none (key/session preserved) | ErrConnectionLost for in-flight | per policy | Reconnects, ConnClosed | Phase 3/12 suites |
| Reconnect exhaust | budget exceeded | terminal | client Disconnected | transport terminal via LastError | no | ConnFailures | transport tests |
| Server stall (no response) | request deadline | pending failed | none | context.DeadlineExceeded | no | RequestsTimedOut | pending tests |
| RPC timeout | ctx deadline | — | none | DeadlineExceeded | no | same | same |
| Cancellation | ctx done | — | none | Canceled (wins) | no | RequestsCancelled | cancellation tests |
| Close | lifecycle barrier | global teardown | terminal | ErrClosed / ErrEngineClosed | no | — | Close-during-* tests |
| Auth revoked (401 permanent) | auth reject mapping | none | AuthorizationRevoked | ErrAuthorizationRevoked | no | auth counters | TestAuthRevoked |
| Auth unregistered (401 recoverable) | auth reject mapping | re-auth possible | session unauthorized | ErrUnauthorized | no | auth counters | TestAuthKeyUnregisteredIsNotRevocation |
| Session file corruption | load HMAC/magic check | fresh session (caller decision) | none (load fails) | ErrCorrupt | no | — | Phase 5 suite |
| Persistence Save failure | Save error | in-memory state untouched | none | Save error | caller-driven | — | persistence fault tests |
| Update gap (pts) | MessageBox pts rule | hold → deadline → difference | pts frontier advance | none (internal) | deadline-gated | UpdateGaps, DifferenceRequests | Phase 10 suite |
| Channel gap | channel pts rule | getChannelDifference | channel pts advance | none | deadline-gated | ChannelDifferenceRequests | Phase 10 suite |
| Too-long difference | differenceTooLong | pts adopt, resync | pts updated | none | — | DifferenceRequests | MessageBox tests |
| Channel banned | ChannelPrivate/Invalid | state forgotten | channel entry removed | none | no | — | end-channel-difference tests |
| Transfer part failure | RPC error on part | transfer failed | none | ErrPartUploadFailed | no (reference) | transfer failed | Phase 8 suite |
| File reference expired | FILE_REFERENCE_EXPIRED | refresh + one retry | reference rotated | original error if refresh fails | exactly once | — | Phase 13 path |
| Queue overflow (events) | grace expiry | DROP_NEWEST | none | OnSlowConsumer hook | n/a | dropped | slow-consumer tests |
| Queue overflow (rawQ) | full | DROP_OLDEST | none (frontier preserved) | — | n/a | received−accepted | runtime tests |
| Pending limit | registry full | FAIL before wire | none | rpc.ErrPendingLimit | no | PendingLimitHits | Phase 4 suite |
| Subscriber panic | recover per delivery | callback skipped | none | OnSubscriberPanic hook | no | — | Phase 6/7 tests |
| Malformed TL | decode error | drop message / fail request | protocol-error metric | decode error | no | ProtocolErrors | fuzz campaigns |
| Invalid media addressing | location resolver | — | none | ErrMediaLocation | no | — | Phase 13 tests |
| Invalid message id | mutation validation | — | none | ErrInvalidPeer | no | — | Phase 12 tests |
| Invalid peer (no hash) | resolution | — | none | ErrAccessHashUnavailable | no | — | Phase 11 tests |
