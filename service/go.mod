module github.com/Skryldev/suromesh/service

go 1.26.0

// DEVELOPMENT DEPENDENCY (pre-publication): the SDK is consumed from the
// adjacent working tree. This replace directive is ISOLATED to this file
// and MUST be removed before any release build — the release policy is
// enforced by service/scripts/check-release.sh (fails whenever a replace
// directive is present) and by the service CI "release-policy" job.
// Removal procedure at publication time:
//   1. Tag and publish the SDK module (github.com/Skryldev/suromesh).
//   2. Delete the replace line below.
//   3. Pin the released SDK version in require.
//   4. Run service/scripts/check-release.sh — it must pass.
require github.com/Skryldev/suromesh v0.0.0

require (
	github.com/jackc/pgx/v5 v5.7.1
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261118221215-c8830f628b3f
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/Skryldev/suromesh => ../
