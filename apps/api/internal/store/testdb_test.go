//go:build integration

package store_test

// Adapters between the package's tests and the shared harness.
//
// The harness itself — container, provisioning, migrations, the database
// identities and the two-tenant seed — lives in internal/testsupport/pgtest, so a second
// integration suite (auth, in #8) gets the same fixtures instead of a second
// opinion about what "two tenants with overlapping data" means.
//
// Three identities, deliberately. Queries under test run as the serving role
// (collabboard_app: no superuser, no BYPASSRLS, owns nothing), which is the
// identity a request actually uses — identity_test.go asserts that rather than
// assuming it. Seeding runs as the container's bootstrap superuser, because
// seeding is precisely the thing every policy-bound role is forbidden from
// doing. The third, collabboard_owner, applied the migrations and is asserted
// about rather than used: provisioning_test.go checks that it is subject to the
// policies it installed.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
)

// tenantFixture is one tenant's seeded data. Aliased rather than redeclared so
// the tests read the same as they did before the harness existed.
type tenantFixture = pgtest.Tenant

// newPool opens a pool as the serving role.
func newPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()

	return testDB.AppPool(t, maxConns)
}

// newSuperuserPool opens a pool as the container's bootstrap superuser, used
// only to seed fixtures and to read the catalog.
//
// Not the schema owner, which since issue #14 is a different, non-superuser
// role: FORCE ROW LEVEL SECURITY applies to that one, so it can neither insert
// the fixtures nor delete them. [newSchemaOwnerPool] is how a test asks for it,
// and the only tests that should are the ones asserting what it cannot do.
func newSuperuserPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	return testDB.SuperuserPool(t, 2)
}

// newSchemaOwnerPool opens a pool as collabboard_owner, the role that applied
// the migrations. Nothing seeds through it — see [newSuperuserPool].
func newSchemaOwnerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	return testDB.SchemaOwnerPool(t, 2)
}

// seedTenants creates two tenants plus a user who belongs to both.
func seedTenants(t *testing.T, superuser *pgxpool.Pool) (a, b tenantFixture, sharedEmail string) {
	t.Helper()

	f := pgtest.SeedTenants(t, superuser)

	return f.A, f.B, f.SharedEmail
}

// seedFixture is seedTenants for the tests that need the shared user's id as
// well as the two tenants — the pre-tenant organization list, whose whole claim
// is that one user's memberships span both.
func seedFixture(t *testing.T, superuser *pgxpool.Pool) pgtest.Fixture {
	t.Helper()

	return pgtest.SeedTenants(t, superuser)
}

// inTenantTx runs fn inside a transaction scoped to tenantID, exactly the way
// store.WithTenant does, and then rolls it back.
//
// It exists for the assertions that cannot go through the generated querier:
// the isolation matrix has to speak to every tenant-scoped table, and the sqlc
// query set is deliberately representative rather than exhaustive. The rollback
// is what lets a subtest attempt a cross-tenant DELETE without depending on the
// policy having stopped it — if the policy ever failed, the fixture still
// survives for the assertion that reports it.
func inTenantTx(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, fn func(tx pgx.Tx)) {
	t.Helper()

	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning transaction: %v", err)
	}

	defer func() {
		if rerr := tx.Rollback(ctx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			t.Errorf("rolling back: %v", rerr)
		}
	}()

	// The same statement store.WithTenant issues: SET LOCAL semantics with the
	// tenant travelling as a bind parameter.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		t.Fatalf("setting tenant context: %v", err)
	}

	fn(tx)
}
