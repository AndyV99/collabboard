// Package pgtest is the integration-test harness: a throwaway Postgres in a
// container, migrated with the real migrations, and reachable under both of the
// database identities the isolation model depends on.
//
// # Why a container rather than the compose stack
//
// The vault's Standards/Testing Strategy.md is blunt about it — "don't mock the
// database in integration tests" — and tenant isolation is the case where that
// matters most, because row-level security is a database behaviour that a mock
// cannot have. Pointing the suite at a long-lived local Postgres would be
// almost as bad in a different way: the schema would be whatever the last
// person's migrations left behind, and CI would need a service someone
// remembered to provision. A container per run is migrated from empty every
// time, so the schema under test is exactly what is in migrations/.
//
// # Two identities, and why the app one is the whole point
//
// [DB.OwnerDSN] is the role that owns the schema. It runs migrations and seeds
// fixtures, and it is the *wrong* identity to test with: row-level security is
// bypassed by superusers, by BYPASSRLS roles and — without FORCE — by the table
// owner. A suite that connects as the owner passes every isolation assertion
// while proving nothing.
//
// [DB.AppDSN] is collabboard_app, the role the API actually serves with. That
// is the only identity the policies apply to, so it is the only identity worth
// asserting against. See docs/adr/0001-tenant-isolation.md, and
// store/identity_test.go, which asserts the distinction in-test rather than
// trusting this comment.
//
// # Build tag
//
// Everything else in this package is behind the `integration` build tag: it
// needs a Docker daemon and takes seconds rather than milliseconds. This file
// carries no tag so that `go build ./...` and `go vet ./...` still find a
// package here instead of failing with "build constraints exclude all Go
// files".
//
//	go test ./...                    # unit loop: no Docker, milliseconds
//	go test -tags=integration ./...  # brings up Postgres, runs everything
package pgtest
