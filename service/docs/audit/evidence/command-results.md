# SuroMesh Service — Audit Command Evidence

Executed: 2026-11-22 | Go 1.26.3 windows/amd64 | no live account (offline scope)

## 1. Service suite (unit + integration-skip)
?   	github.com/Skryldev/suromesh/service/cmd/service	[no test files]
?   	github.com/Skryldev/suromesh/service/gen/suromesh/service/v1	[no test files]
ok  	github.com/Skryldev/suromesh/service/internal/app	1.616s
ok  	github.com/Skryldev/suromesh/service/internal/application	1.732s
ok  	github.com/Skryldev/suromesh/service/internal/config	1.321s
ok  	github.com/Skryldev/suromesh/service/internal/domain	1.469s
?   	github.com/Skryldev/suromesh/service/internal/grpcapi/errors	[no test files]
?   	github.com/Skryldev/suromesh/service/internal/grpcapi/handlers	[no test files]
?   	github.com/Skryldev/suromesh/service/internal/grpcapi/interceptors	[no test files]
ok  	github.com/Skryldev/suromesh/service/internal/grpcapi/server	0.450s
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/cryptography	1.315s
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/persistence	1.865s
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/upstream	1.528s
ok  	github.com/Skryldev/suromesh/service/internal/platform/logging	0.873s
?   	github.com/Skryldev/suromesh/service/internal/ports	[no test files]
?   	github.com/Skryldev/suromesh/service/internal/testkit	[no test files]

## 2. Service race suite
?   	github.com/Skryldev/suromesh/service/cmd/service	[no test files]
?   	github.com/Skryldev/suromesh/service/gen/suromesh/service/v1	[no test files]
ok  	github.com/Skryldev/suromesh/service/internal/app	1.753s
ok  	github.com/Skryldev/suromesh/service/internal/application	1.907s
ok  	github.com/Skryldev/suromesh/service/internal/config	1.525s
ok  	github.com/Skryldev/suromesh/service/internal/domain	1.621s
?   	github.com/Skryldev/suromesh/service/internal/grpcapi/errors	[no test files]
?   	github.com/Skryldev/suromesh/service/internal/grpcapi/handlers	[no test files]
?   	github.com/Skryldev/suromesh/service/internal/grpcapi/interceptors	[no test files]
ok  	github.com/Skryldev/suromesh/service/internal/grpcapi/server	2.523s
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/cryptography	1.236s
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/persistence	1.520s
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/upstream	1.408s
ok  	github.com/Skryldev/suromesh/service/internal/platform/logging	1.235s
?   	github.com/Skryldev/suromesh/service/internal/ports	[no test files]
?   	github.com/Skryldev/suromesh/service/internal/testkit	[no test files]

## 3. Conformance tests (adapter)
=== RUN   TestSessionBlob_ExportImportSymmetry
--- PASS: TestSessionBlob_ExportImportSymmetry (0.00s)
=== RUN   TestSessionBlob_InvalidInputs
=== RUN   TestSessionBlob_InvalidInputs/not_json
=== RUN   TestSessionBlob_InvalidInputs/wrong_version
=== RUN   TestSessionBlob_InvalidInputs/bad_base64_key
=== RUN   TestSessionBlob_InvalidInputs/short_key
=== RUN   TestSessionBlob_InvalidInputs/empty
--- PASS: TestSessionBlob_InvalidInputs (0.00s)
=== RUN   TestSessionBlob_RevocationReflected
--- PASS: TestSessionBlob_RevocationReflected (0.00s)
=== RUN   TestTranslate_ErrorMappingTable
=== RUN   TestTranslate_ErrorMappingTable/context_canceled
=== RUN   TestTranslate_ErrorMappingTable/context_deadline
=== RUN   TestTranslate_ErrorMappingTable/wrapped_cancel
=== RUN   TestTranslate_ErrorMappingTable/flood_wait
=== RUN   TestTranslate_ErrorMappingTable/auth_key_unregistered
=== RUN   TestTranslate_ErrorMappingTable/session_revoked
=== RUN   TestTranslate_ErrorMappingTable/session_expired
=== RUN   TestTranslate_ErrorMappingTable/file_ref_expired
=== RUN   TestTranslate_ErrorMappingTable/peer_id_invalid
=== RUN   TestTranslate_ErrorMappingTable/peer_hash_invalid
=== RUN   TestTranslate_ErrorMappingTable/channel_private
=== RUN   TestTranslate_ErrorMappingTable/message_id_invalid
=== RUN   TestTranslate_ErrorMappingTable/generic_rpc_error
=== RUN   TestTranslate_ErrorMappingTable/connection_lost_wrapper
=== RUN   TestTranslate_ErrorMappingTable/client_closed
=== RUN   TestTranslate_ErrorMappingTable/client_not_ready
=== RUN   TestTranslate_ErrorMappingTable/file_ref_expired_sentinel
--- PASS: TestTranslate_ErrorMappingTable (0.00s)
=== RUN   TestTranslate_TransportAndNetwork
--- PASS: TestTranslate_TransportAndNetwork (0.00s)
=== RUN   TestConvertUpdate_Families
--- PASS: TestConvertUpdate_Families (0.00s)
=== RUN   TestConvertMessage_Fidelity
--- PASS: TestConvertMessage_Fidelity (0.00s)
=== RUN   TestPeerConversion_RoundTrip
--- PASS: TestPeerConversion_RoundTrip (0.00s)
PASS
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/upstream	0.941s

## 4. gRPC contract tests
--- PASS: TestAuthN_MissingKeyFails (0.00s)
--- PASS: TestAuthN_BadKeyFails (0.00s)
--- PASS: TestAuthZ_MissingPermissionDenied (0.00s)
--- PASS: TestAccounts_BindListGet (0.00s)
--- PASS: TestAuthFlow_SendCodeRequiresLiveRunner (0.00s)
--- PASS: TestHealth_ServedWithoutCredentials (0.00s)
--- PASS: TestManager_Isolation_TenantScopedGet (0.00s)
--- PASS: TestStreamUpdates_DeliversAndSequences (0.15s)
--- PASS: TestStreamUpdates_ClientCancelEndsCleanly (0.00s)
--- PASS: TestStreamHistory_UnscriptedFailsHonest (0.00s)
--- PASS: TestMediaUpload_FirstFrameMustBeMetadata (0.00s)
--- PASS: TestMediaUpload_MetadataValidation (0.00s)
--- PASS: TestMediaUpload_SizeEnforcement (0.00s)
--- PASS: TestMediaUpload_UnscriptedUpstreamFailsHonest (0.00s)
--- PASS: TestDeadline_CallerDeadlineWins (0.00s)
ok  	github.com/Skryldev/suromesh/service/internal/grpcapi/server	0.308s

## 5. Domain tests
--- PASS: TestDomainDependencyBoundary (0.07s)
--- PASS: TestPeerID_ParseRoundTrip (0.00s)
--- PASS: TestPeerID_InvalidInputs (0.00s)
--- PASS: TestPeerRef_StringForms (0.00s)
--- PASS: TestErrorClassification_FullTaxonomy (0.00s)
--- PASS: TestErrorClassification_SentinelsAreDistinct (0.00s)
--- PASS: TestFloodWait_UnwrapAndSeconds (0.00s)
--- PASS: TestFormatID_DecimalStrings (0.00s)
--- PASS: TestStateStrings_Stable (0.00s)
ok  	github.com/Skryldev/suromesh/service/internal/domain	0.645s

## 6. Crypto tests
--- PASS: TestEnvelope_SealUnsealRoundTrip (0.00s)
--- PASS: TestEnvelope_TamperFailsClosed (0.00s)
--- PASS: TestEnvelope_WrongKeyFailsClosed (0.00s)
--- PASS: TestEnvelope_RotationReWrap (0.00s)
--- PASS: TestFileKeyProvider_Errors (0.00s)
--- PASS: TestKMSProvider_FailsClosed (0.00s)
--- PASS: TestEnvelope_EmptyPlaintextRejected (0.00s)
ok  	github.com/Skryldev/suromesh/service/internal/infrastructure/cryptography	0.550s

## 7. Application tests (runner/manager/hub)
--- PASS: TestStartAll_OrdersAndStartsEverything (0.00s)
--- PASS: TestStartAll_FailureStopsStartedPrefixInReverse (0.00s)
--- PASS: TestStopAll_ReverseOrder (0.00s)
--- PASS: TestStopAll_JoinsErrorsAndStopsEverything (0.00s)
--- PASS: TestStopAll_RespectsContextDeadline (0.10s)
--- PASS: TestRunner_StartWithSessionRestore (0.00s)
--- PASS: TestRunner_MutationSerialization (0.09s)
--- PASS: TestRunner_ReadsRunConcurrently (0.03s)
--- PASS: TestRunner_SessionPersistedOnUpstreamSignal (0.00s)
--- PASS: TestRunner_StopDuringBlockedMutation (0.02s)
--- PASS: TestRunner_SubscribeHubDelivery (0.00s)
--- PASS: TestRunner_SlowSubscriberDropped (0.00s)
--- PASS: TestManager_DuplicateRunnerRejected (0.00s)
--- PASS: TestManager_TenantScopedGet (0.00s)
--- PASS: TestStress_MutationThroughputAndOrdering (0.00s)
--- PASS: TestStress_QueueSaturationRejectsDeterministically (0.13s)
--- PASS: TestStress_ConcurrentSubscribeUnsubscribe (0.00s)
--- PASS: TestStress_MultipleSubscribersAllReceive (0.00s)
--- PASS: TestStress_StopAllIdempotent (0.00s)
--- PASS: TestStress_RunnerStopTwice (0.00s)
ok  	github.com/Skryldev/suromesh/service/internal/application	0.895s
