//go:build integration

package store_test

// The role model itself, asserted against a live database.
//
// # What this file is for
//
// Every other integration test here proves something about the *schema* —
// policies, grants, function ownership. This one proves something about the
// *identity the schema was installed by*, which is the half issue #14 found
// missing. ADR 0001 names the failure mode and calls it the superuser/owner
// trap: RLS is bypassed by superusers, by BYPASSRLS roles, and by a table's
// owner unless FORCE ROW LEVEL SECURITY is set. For the first five migrations
// the owner *was* the container's bootstrap superuser, so FORCE was never
// exercised by the role it exists for, and three of those migrations had come
// to depend on privileges no correctly provisioned owner has.
//
// The migrations are the fix; these are the assertions that stop it coming
// back. They are deliberately about collabboard_owner rather than
// collabboard_app: the app role is covered thoroughly elsewhere, and the owner
// is the role nobody was watching.
//
// # Why the owner is worth testing at all when nothing connects as it
//
// Two reasons. It is the identity a misconfigured POSTGRES_USER would hand the
// API — the single environment variable between "the policies apply" and "they
// do not" — so what it can see is the blast radius of one typo. And it is the
// role that would still be exempt if someone reverted FORCE on a table, which
// is a one-line change that no other test in this package would notice.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
)

// roleAttributes is the subset of pg_roles that decides whether row-level
// security means anything for a role.
type roleAttributes struct {
	super       bool
	bypassRLS   bool
	createDB    bool
	replication bool
	canLogin    bool
}

func readRoleAttributes(t *testing.T, pool *pgxpool.Pool, role string) roleAttributes {
	t.Helper()

	var attrs roleAttributes

	err := pool.QueryRow(context.Background(), `
		SELECT r.rolsuper, r.rolbypassrls, r.rolcreatedb, r.rolreplication, r.rolcanlogin
		FROM pg_catalog.pg_roles r
		WHERE r.rolname = $1`, role).
		Scan(&attrs.super, &attrs.bypassRLS, &attrs.createDB, &attrs.replication, &attrs.canLogin)
	if err != nil {
		t.Fatalf("reading attributes of role %s: %v", role, err)
	}

	return attrs
}

// TestSchemaOwnerHoldsNoAttributeThatBypassesRLS is acceptance criterion one of
// issue #14, asserted rather than assumed.
//
// CREATEROLE is deliberately absent from this list and deliberately held by the
// owner: migrations 00001, 00004 and 00005 create the other three roles, and
// PostgreSQL 16 will not let a CREATEROLE role grant SUPERUSER, BYPASSRLS or
// REPLICATION, so it cannot manufacture a way out of the constraints below.
func TestSchemaOwnerHoldsNoAttributeThatBypassesRLS(t *testing.T) {
	attrs := readRoleAttributes(t, newSuperuserPool(t), pgtest.SchemaOwnerRole)

	t.Logf("%s: rolsuper=%t rolbypassrls=%t rolcreatedb=%t rolreplication=%t",
		pgtest.SchemaOwnerRole, attrs.super, attrs.bypassRLS, attrs.createDB, attrs.replication)

	for _, tc := range []struct {
		attribute string
		held      bool
		why       string
	}{
		{"SUPERUSER", attrs.super, "a superuser is exempt from every policy in this schema"},
		{"BYPASSRLS", attrs.bypassRLS, "the same exemption under a different name"},
		{"CREATEDB", attrs.createDB, "this role owns one database and has no business making more"},
		{"REPLICATION", attrs.replication, "a logical replication slot is a copy of every tenant's rows"},
	} {
		if tc.held {
			t.Errorf("%s holds %s: %s", pgtest.SchemaOwnerRole, tc.attribute, tc.why)
		}
	}

	// The one attribute it must hold. A NOLOGIN owner would make every check
	// above pass while no migration could ever run, which is a different bug
	// with the same test output.
	if !attrs.canLogin {
		t.Errorf("%s cannot log in; migrations connect as it", pgtest.SchemaOwnerRole)
	}
}

// TestSchemaOwnerOwnsEveryTableItMigrated checks the other half of the trap.
//
// Attributes are only half of "row-level security applies to me": the other
// half is FORCE, which only matters for the owner. If ownership had stayed with
// the superuser while migrations ran as collabboard_owner, every assertion
// below about what the owner cannot see would pass for the wrong reason — a
// non-owner is subject to policies whether or not FORCE is set.
func TestSchemaOwnerOwnsEveryTableItMigrated(t *testing.T) {
	rows, err := newSuperuserPool(t).Query(context.Background(), `
		SELECT c.relnamespace::regnamespace::text || '.' || c.relname,
		       c.relowner::regrole::text
		FROM pg_catalog.pg_class c
		WHERE c.relkind = 'r'
		  AND c.relnamespace::regnamespace::text IN ('public', 'auth')
		ORDER BY 1`)
	if err != nil {
		t.Fatalf("reading table ownership: %v", err)
	}

	defer rows.Close()

	var checked int

	for rows.Next() {
		var table, owner string

		if err := rows.Scan(&table, &owner); err != nil {
			t.Fatalf("scanning table ownership: %v", err)
		}

		checked++

		if owner != pgtest.SchemaOwnerRole {
			t.Errorf("%s is owned by %q, want %q", table, owner, pgtest.SchemaOwnerRole)
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading table ownership: %v", err)
	}

	t.Logf("checked ownership of %d tables", checked)

	// The goose bookkeeping table alone would be nine short of the schema. A
	// zero here would mean the query matched nothing and the loop above
	// asserted nothing.
	if checked == 0 {
		t.Fatal("found no tables in public or auth; the ownership check asserted nothing")
	}
}

// TestSchemaOwnerIsSubjectToForcedRLS is acceptance criterion two.
//
// row_security_active() is the direct question rather than a proxy for it: is
// RLS being enforced against me, right now, for this table. It is false for a
// superuser, false for a BYPASSRLS role, false for an owner of a table that is
// ENABLEd but not FORCEd, and false in a session that has set row_security =
// off. Only one combination makes it true, and that combination is the point of
// this issue. Migration 00006 makes the same call for the same reason.
func TestSchemaOwnerIsSubjectToForcedRLS(t *testing.T) {
	owner := newSchemaOwnerPool(t)
	superuser := newSuperuserPool(t)

	for _, table := range []string{
		"public.organizations",
		"public.users",
		"public.memberships",
		"public.projects",
		"public.boards",
		"public.columns",
		"public.cards",
		"auth.user_credentials",
	} {
		t.Run(table, func(t *testing.T) {
			var active bool

			if err := owner.QueryRow(context.Background(),
				`SELECT pg_catalog.row_security_active($1::text)`, table).Scan(&active); err != nil {
				t.Fatalf("asking whether RLS is active on %s: %v", table, err)
			}

			if !active {
				t.Errorf("row-level security is not enforced against %s for %s; FORCE ROW LEVEL SECURITY is what makes the owner subject to its own policies",
					table, pgtest.SchemaOwnerRole)
			}

			// The control. Without it, a true above could mean "RLS is on for
			// everyone", which would be a different and much worse schema —
			// the superuser has to still be exempt, because that is the
			// documented Postgres behaviour this whole role model works
			// around.
			var activeForSuperuser bool

			if err := superuser.QueryRow(context.Background(),
				`SELECT pg_catalog.row_security_active($1::text)`, table).Scan(&activeForSuperuser); err != nil {
				t.Fatalf("asking whether RLS is active on %s for the superuser: %v", table, err)
			}

			if activeForSuperuser {
				t.Errorf("row_security_active(%s) is true for %s as well, so the assertion above says nothing about %s",
					table, pgtest.SuperuserRole, pgtest.SchemaOwnerRole)
			}
		})
	}
}

// TestSchemaOwnerWithNoTenantContextSeesNothing is acceptance criterion two
// stated the way an operator would check it: connect as the owner, select, get
// nothing.
//
// It runs against seeded data, so "zero rows" is a claim about the policies
// rather than about an empty table — the control at the end reads the same rows
// through the identity that is allowed to, and would fail if the fixture had
// not landed.
func TestSchemaOwnerWithNoTenantContextSeesNothing(t *testing.T) {
	superuser := newSuperuserPool(t)
	fixture := seedFixture(t, superuser)

	owner := newSchemaOwnerPool(t)

	for _, q := range []struct {
		table string
		sql   string
		arg   any
	}{
		{"organizations", `SELECT count(*) FROM organizations WHERE id = $1`, fixture.A.TenantID},
		{"users", `SELECT count(*) FROM users WHERE id = $1`, fixture.A.MemberID},
		{"memberships", `SELECT count(*) FROM memberships WHERE tenant_id = $1`, fixture.A.TenantID},
		{"projects", `SELECT count(*) FROM projects WHERE id = $1`, fixture.A.ProjectID},
		{"boards", `SELECT count(*) FROM boards WHERE id = $1`, fixture.A.BoardID},
		{"columns", `SELECT count(*) FROM columns WHERE id = $1`, fixture.A.ColumnID},
		{"cards", `SELECT count(*) FROM cards WHERE id = $1`, fixture.A.CardID},
	} {
		t.Run(q.table, func(t *testing.T) {
			var count int

			if err := owner.QueryRow(context.Background(), q.sql, q.arg).Scan(&count); err != nil {
				t.Fatalf("counting %s as %s: %v", q.table, pgtest.SchemaOwnerRole, err)
			}

			if count != 0 {
				t.Errorf("%s sees %d row(s) in %s with app.tenant_id unset; the migration role is not supposed to be able to read tenant data",
					pgtest.SchemaOwnerRole, count, q.table)
			}
		})
	}

	// The control: the same rows exist and are readable by the identity that is
	// allowed to read them. Without this, every count above would be zero for a
	// misspelled fixture id just as happily.
	var seeded int

	if err := superuser.QueryRow(context.Background(),
		`SELECT count(*) FROM cards WHERE id = $1`, fixture.A.CardID).Scan(&seeded); err != nil {
		t.Fatalf("reading the control row: %v", err)
	}

	if seeded != 1 {
		t.Fatalf("the fixture card is not there (%d rows); the zero counts above prove nothing", seeded)
	}
}

// TestSchemaOwnerHoldsTheIdentityRolesWithoutInheritingThem is the assertion
// behind part two of migration 00006, and the one that would have been easiest
// to miss.
//
// Migrations 00004 and 00005 grant the migration role membership in the two
// function-owning roles, because ALTER FUNCTION ... OWNER requires the caller to
// be able to SET ROLE to the new owner. A plain GRANT carries SET *and* INHERIT,
// and INHERIT is the expensive half: the pre-tenant policies are attached
// `TO collabboard_identity` with `USING (true)`, and PostgreSQL applies a
// policy's role list to anyone holding the privileges of a named role. So an
// inheriting migration role could read the entire global user directory with no
// tenant context and no SET ROLE — which is exactly what the credentials a
// misconfigured POSTGRES_USER would hand the API.
//
// SET without INHERIT is the narrower grant that still does everything the
// migrations need.
func TestSchemaOwnerHoldsTheIdentityRolesWithoutInheritingThem(t *testing.T) {
	owner := newSchemaOwnerPool(t)

	for _, role := range []string{pgtest.IdentityRole, pgtest.CredentialsRole} {
		t.Run(role, func(t *testing.T) {
			var inherits, canSetRole bool

			if err := owner.QueryRow(context.Background(), `
				SELECT pg_catalog.pg_has_role(current_user, $1, 'USAGE'),
				       pg_catalog.pg_has_role(current_user, $1, 'SET')`, role).
				Scan(&inherits, &canSetRole); err != nil {
				t.Fatalf("reading %s membership: %v", role, err)
			}

			if inherits {
				t.Errorf("%s inherits the privileges of %s, so every policy attached TO %s applies to it ambiently",
					pgtest.SchemaOwnerRole, role, role)
			}

			// The control, and the reason this is not simply a REVOKE:
			// without SET, ALTER FUNCTION ... OWNER in 00004 and 00005 fails
			// and the whole chain stops applying.
			if !canSetRole {
				t.Errorf("%s cannot SET ROLE to %s; migrations 00004 and 00005 need that to reassign function ownership",
					pgtest.SchemaOwnerRole, role)
			}
		})
	}
}

// TestAppRoleCannotTurnRowSecurityOff is the direct form of "the app role still
// cannot bypass RLS", asked as the app role against the provisioning this issue
// introduces.
//
// `SET row_security = off` is the one knob a session can reach for. PostgreSQL
// honours it only for roles that would be exempt anyway; for anyone else the
// next query touching a policy-bearing table fails outright with 42501 rather
// than returning rows. So an error here is the pass, and rows are the failure.
func TestAppRoleCannotTurnRowSecurityOff(t *testing.T) {
	superuser := newSuperuserPool(t)
	fixture := seedFixture(t, superuser)

	ctx := context.Background()
	app := newPool(t, 1)

	conn, err := app.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring an app connection: %v", err)
	}

	defer conn.Release()

	if _, err := conn.Exec(ctx, `SET row_security = off`); err != nil {
		t.Fatalf("SET row_security = off as %s: %v", pgtest.AppRole, err)
	}

	var count int

	err = conn.QueryRow(ctx, `SELECT count(*) FROM cards WHERE id = $1`, fixture.A.CardID).Scan(&count)
	if err == nil {
		t.Fatalf("%s read %d row(s) from cards with row_security off; the role can bypass row-level security",
			pgtest.AppRole, count)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want a Postgres error, got %T: %v", err, err)
	}

	// 42501 insufficient_privilege — "query would be affected by row-level
	// security policy for table". Asserted specifically so that a syntax error
	// or a dropped table could not be mistaken for the policy holding.
	if pgErr.Code != insufficientPrivilege {
		t.Errorf("want SQLSTATE %s, got %s (%s)", insufficientPrivilege, pgErr.Code, pgErr.Message)
	}

	t.Logf("%s: %s", pgErr.Code, pgErr.Message)
}

// TestSchemaOwnerCannotTurnRowSecurityOffEither closes the same door on the
// role that owns the tables.
//
// This one is not obvious. The owner *can* say `ALTER TABLE ... NO FORCE ROW
// LEVEL SECURITY`, which no database privilege can stop — what stops it is that
// only reviewed migrations connect as this role. But `SET row_security = off`
// is a session setting rather than DDL, and it is worth knowing that the owner
// gets the same 42501 an ordinary role gets: the escape hatch is a schema
// change with an audit trail, not a connection option.
func TestSchemaOwnerCannotTurnRowSecurityOffEither(t *testing.T) {
	superuser := newSuperuserPool(t)
	fixture := seedFixture(t, superuser)

	ctx := context.Background()

	conn, err := newSchemaOwnerPool(t).Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a schema owner connection: %v", err)
	}

	defer conn.Release()

	if _, err := conn.Exec(ctx, `SET row_security = off`); err != nil {
		t.Fatalf("SET row_security = off as %s: %v", pgtest.SchemaOwnerRole, err)
	}

	var count int

	err = conn.QueryRow(ctx, `SELECT count(*) FROM cards WHERE id = $1`, fixture.A.CardID).Scan(&count)
	if err == nil {
		t.Fatalf("%s read %d row(s) from cards with row_security off", pgtest.SchemaOwnerRole, count)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("want a Postgres error, got %T: %v", err, err)
	}

	if pgErr.Code != insufficientPrivilege {
		t.Errorf("want SQLSTATE %s, got %s (%s)", insufficientPrivilege, pgErr.Code, pgErr.Message)
	}
}

// TestMigrationsRefuseToRunAsAnExemptRole exercises migration 00006 against the
// exact mistake it exists for: applying the chain as the bootstrap superuser.
//
// The container's database is already migrated, so goose has nothing to apply
// and 00006 would not run again. The check itself is the thing under test, so
// this evaluates it directly as the superuser and asserts it raises — the same
// SQL, the same identity, without needing a second container.
func TestMigrationsRefuseToRunAsAnExemptRole(t *testing.T) {
	_, err := newSuperuserPool(t).Exec(context.Background(), migrationRoleGuardSQL)
	if err == nil {
		t.Fatalf("the migration-role guard passed as %s; migration 00006 would let a superuser apply the chain",
			pgtest.SuperuserRole)
	}

	t.Logf("as %s: %v", pgtest.SuperuserRole, err)

	// The same statement as the schema owner is the control. If it failed for
	// both, the guard would be rejecting everything and the assertion above
	// would be meaningless.
	if _, err := newSchemaOwnerPool(t).Exec(context.Background(), migrationRoleGuardSQL); err != nil {
		t.Errorf("the migration-role guard rejected %s, which is the role migrations are supposed to run as: %v",
			pgtest.SchemaOwnerRole, err)
	}
}

// migrationRoleGuardSQL is the body of migration 00006.
//
// Restated here rather than parsed out of the embedded migration, because the
// migration file is annotated goose SQL and splitting it in a test would be a
// second, worse goose. The duplication is real and bounded: if the two drift,
// this test starts asserting something the deploy does not do, so the comment
// in 00006 points back here.
const migrationRoleGuardSQL = `
DO $$
BEGIN
    IF pg_catalog.row_security_active('public.users') THEN
        RETURN;
    END IF;

    RAISE EXCEPTION 'migrations are running as %, which row-level security is not enforced against', CURRENT_USER;
END
$$`
