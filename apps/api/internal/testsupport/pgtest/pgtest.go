// Package pgtest is the integration-test harness: a throwaway Postgres in a
// container, provisioned and migrated the way a deploy provisions and migrates,
// and reachable under each of the database identities the isolation model
// depends on.
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
// # Three identities, and why picking the wrong one is the failure mode
//
// [DB.SuperuserDSN] is the cluster's bootstrap superuser. It exists to do the
// two things nothing else can: create the schema owner, and seed fixtures. It
// is the *wrong* identity to test with — row-level security is not enforced
// against a superuser at all — so nothing under test runs through it and no
// assertion is made through it.
//
// [DB.SchemaOwnerDSN] is collabboard_owner: a non-superuser with no BYPASSRLS
// that owns every table and applies the migrations. Before issue #14 this role
// did not exist and migrations ran as the superuser, which meant the whole
// migration chain had quietly come to depend on privileges no correctly
// provisioned owner has. It is also the identity FORCE ROW LEVEL SECURITY
// exists for, so it is worth asserting against: with no tenant context it must
// see nothing.
//
// [DB.AppDSN] is collabboard_app, the role the API actually serves with. It is
// the identity almost every test should use, because it is the one a request
// runs under. See docs/adr/0001-tenant-isolation.md and
// docs/adr/0005-database-role-provisioning.md, and store/identity_test.go and
// store/provisioning_test.go, which assert the distinctions in-test rather than
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
