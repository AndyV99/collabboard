//go:build integration

package store_test

// The other half of the pre-tenant claim: not "this works" but "this is all
// that works".
//
// pretenant_unit_test.go proves the Go side — the identity querier has four
// methods and a tenant-scoped query does not compile against it. That is the
// guarantee for code written in this repo. This file proves the database side,
// which is the guarantee that survives a future function body, a hand-written
// query, or someone deciding the compile error is inconvenient: even acting as
// the role that owns the identity functions, tenant-scoped tables are not
// readable, because the privilege was never granted.
//
// The two are independent. Either one alone would be a convention.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
)

// identityVisibleTables is what the pre-tenant path is allowed to reach.
// Everything else in the schema must be unreachable, and the list of
// "everything else" comes from the catalog rather than from this file, so a
// table added by a future migration is covered the moment it exists.
var identityVisibleTables = []string{"organizations", "memberships", "users"}

// identityFunctions is the complete set of SECURITY DEFINER functions the app
// role may execute — the pre-tenant path's entire surface, spelled out so that
// a fifth one cannot appear without this test failing and someone explaining
// why the operation is genuinely pre-tenant.
var identityFunctions = []string{
	"identity_create_user",
	"identity_find_user_by_email",
	"identity_list_user_organizations",
	"identity_resolve_user_id_by_email",
}

// TestTheIdentityRoleCannotTouchAnyTenantScopedTable is the headline: the role
// the SECURITY DEFINER functions run as holds no privilege of any kind on any
// tenant-scoped table, on any column.
//
// has_any_column_privilege as well as has_table_privilege, because a
// column-level grant would leave the table-level answer false while still
// exposing data — which is exactly how the identity role's *own* grants on
// users are expressed, so it is not a hypothetical shape.
func TestTheIdentityRoleCannotTouchAnyTenantScopedTable(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)

	tables := tableNames(t, owner)

	checked := 0

	for _, table := range tables {
		if slices.Contains(identityVisibleTables, table) {
			continue
		}

		checked++

		// DELETE and TRUNCATE exist only at table level; the rest can also be
		// granted per column, and has_any_column_privilege rejects the two that
		// cannot. Hence the second, shorter list rather than one loop.
		for _, verb := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES"} {
			var atTable bool

			if err := owner.QueryRow(ctx,
				`SELECT has_table_privilege($1, $2, $3)`,
				pgtest.IdentityRole, table, verb).Scan(&atTable); err != nil {
				t.Fatalf("asking the catalog about %s on %s: %v", verb, table, err)
			}

			t.Logf("%s on table %s: %t", verb, table, atTable)

			if atTable {
				t.Errorf("%s holds %s on tenant-scoped table %s; the pre-tenant path can reach tenant data",
					pgtest.IdentityRole, verb, table)
			}
		}

		for _, verb := range []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"} {
			var atColumn bool

			if err := owner.QueryRow(ctx,
				`SELECT has_any_column_privilege($1, $2, $3)`,
				pgtest.IdentityRole, table, verb).Scan(&atColumn); err != nil {
				t.Fatalf("asking the catalog about column %s on %s: %v", verb, table, err)
			}

			t.Logf("%s on any column of %s: %t", verb, table, atColumn)

			if atColumn {
				t.Errorf("%s holds %s on a column of tenant-scoped table %s; a column grant leaves the table-level answer false while still exposing data",
					pgtest.IdentityRole, verb, table)
			}
		}
	}

	t.Logf("checked %d tenant-scoped tables (schema has %v, identity may see %v)", checked, tables, identityVisibleTables)

	if checked == 0 {
		t.Fatal("no tenant-scoped tables were checked; this test would pass on an empty schema")
	}
}

// TestASecurityDefinerFunctionCannotReadTenantData is the same claim at runtime
// rather than from the catalog, and it is the one worth reading.
//
// It builds the exact thing this design is afraid of — a SECURITY DEFINER
// function owned by the identity role that selects from a tenant-scoped table —
// grants the app role EXECUTE on it, and calls it. Postgres refuses. So the
// protection is not "nobody would write that function": it is that the function
// does not work if they do.
func TestASecurityDefinerFunctionCannotReadTenantData(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	app := newPool(t, 1)

	for _, table := range tableNames(t, owner) {
		if slices.Contains(identityVisibleTables, table) {
			continue
		}

		t.Run(table, func(t *testing.T) {
			// Named per table so subtests cannot collide, and dropped by the
			// cleanup below whether the assertion passes or fails.
			probe := "pretenant_probe_" + table

			create := fmt.Sprintf(`
				CREATE FUNCTION public.%s() RETURNS bigint
				LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
				AS 'SELECT count(*) FROM public.%s';
				ALTER FUNCTION public.%s() OWNER TO %s;
				GRANT EXECUTE ON FUNCTION public.%s() TO %s;
			`, probe, table, probe, pgtest.IdentityRole, probe, pgtest.AppRole)

			if _, err := owner.Exec(ctx, create); err != nil {
				t.Fatalf("creating the probe function: %v", err)
			}

			// ctx rather than a fresh background context: it is
			// context.Background() here and is never cancelled, so the cleanup
			// cannot inherit a cancellation and leave the probe behind.
			t.Cleanup(func() {
				if _, derr := owner.Exec(ctx,
					fmt.Sprintf(`DROP FUNCTION IF EXISTS public.%s()`, probe)); derr != nil {
					t.Errorf("dropping the probe function: %v", derr)
				}
			})

			var count int64

			err := app.QueryRow(ctx, fmt.Sprintf(`SELECT public.%s()`, probe)).Scan(&count)
			if err == nil {
				t.Fatalf("a SECURITY DEFINER function owned by %s counted %d rows in %s; the pre-tenant path can be widened into a tenant-data escape hatch",
					pgtest.IdentityRole, count, table)
			}

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("the probe failed with a non-Postgres error: %v", err)
			}

			t.Logf("SECURITY DEFINER count(*) on %s as %s -> SQLSTATE %s: %s",
				table, pgtest.IdentityRole, pgErr.Code, pgErr.Message)

			if pgErr.Code != insufficientPrivilege {
				t.Errorf("the probe on %s was rejected with SQLSTATE %s (%s), want %s — something other than the missing grant stopped it",
					table, pgErr.Code, pgErr.Message, insufficientPrivilege)
			}
		})
	}
}

// TestTheAppRoleMayExecuteExactlyThePreTenantFunctions is the width of the door
// as the database sees it.
//
// The Go side of this is a reflection test over an interface, which a
// hand-written query would sidestep. This one does not care what Go code
// exists: it asks what the serving role is actually permitted to invoke.
func TestTheAppRoleMayExecuteExactlyThePreTenantFunctions(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)

	rows, err := owner.Query(ctx, `
		SELECT p.proname
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public'
		  AND has_function_privilege($1, p.oid, 'EXECUTE')
		  AND p.prosecdef
		ORDER BY p.proname
	`, pgtest.AppRole)
	if err != nil {
		t.Fatalf("listing executable definer functions: %v", err)
	}

	defer rows.Close()

	var executable []string

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning function name: %v", err)
		}

		executable = append(executable, name)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("listing executable definer functions: %v", err)
	}

	t.Logf("%s may execute these SECURITY DEFINER functions: %v", pgtest.AppRole, executable)

	if !slices.Equal(executable, identityFunctions) {
		t.Errorf("%s may execute %v, want exactly %v — the pre-tenant path changed width",
			pgtest.AppRole, executable, identityFunctions)
	}
}

// TestEverySecurityDefinerFunctionIsPinnedAndOwned checks the three ways a
// definer function goes wrong, over every one that exists rather than over the
// four this migration added.
//
//   - Owned by anyone else, and it runs with that role's privileges instead —
//     as the schema owner, which is subject to no policy that matters, or worse
//     as a superuser, which is subject to none at all.
//   - No fixed search_path, and a caller can point `users` at a table they
//     control and have the function operate on it as its owner.
//   - EXECUTE left to PUBLIC, which is the default on a new function, and the
//     door is open to every role in the cluster.
func TestEverySecurityDefinerFunctionIsPinnedAndOwned(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)

	rows, err := owner.Query(ctx, `
		SELECT p.proname,
		       pg_get_userbyid(p.proowner),
		       coalesce(array_to_string(p.proconfig, ','), ''),
		       has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public'
		  AND p.prosecdef
		ORDER BY p.proname
	`)
	if err != nil {
		t.Fatalf("reading definer functions from the catalog: %v", err)
	}

	defer rows.Close()

	seen := 0

	for rows.Next() {
		var (
			name, funcOwner, config string
			publicMayExecute        bool
		)

		if err := rows.Scan(&name, &funcOwner, &config, &publicMayExecute); err != nil {
			t.Fatalf("scanning definer function: %v", err)
		}

		seen++

		t.Logf("%s: owner=%s config=%q public_execute=%t", name, funcOwner, config, publicMayExecute)

		if funcOwner != pgtest.IdentityRole {
			t.Errorf("%s is SECURITY DEFINER but owned by %s; it runs with that role's privileges, not the identity role's",
				name, funcOwner)
		}

		if config == "" {
			t.Errorf("%s is SECURITY DEFINER with no fixed search_path; a caller can choose which tables it operates on", name)
		}

		if publicMayExecute {
			t.Errorf("%s may be executed by PUBLIC; every role in the cluster can act as %s through it", name, funcOwner)
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading definer functions from the catalog: %v", err)
	}

	t.Logf("%d SECURITY DEFINER function(s) in schema public", seen)

	if seen != len(identityFunctions) {
		t.Errorf("schema public has %d SECURITY DEFINER functions, want %d", seen, len(identityFunctions))
	}
}

// TestTheServingRoleCannotAssumeTheIdentityRole closes the last way in.
//
// If collabboard_app could SET ROLE to collabboard_identity, none of the rest of
// this file would matter: it could read the whole user directory with a plain
// SELECT and never touch a function. The identity role also cannot log in, so
// there is no credential for it either.
//
// The migration role *is* expected to be a member — reassigning ownership of a
// function requires it, and handing it back would break rollback while buying
// nothing, since the migration role owns every table and can turn FORCE off. The
// member list is logged rather than asserted empty for that reason, but any
// member that can log in and is not the schema owner is a finding.
func TestTheServingRoleCannotAssumeTheIdentityRole(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)

	var (
		canLogin, super, bypassRLS bool
		appIsMember, appHasUsage   bool
		members                    []string
	)

	err := owner.QueryRow(ctx, `
		SELECT r.rolcanlogin,
		       r.rolsuper,
		       r.rolbypassrls,
		       pg_has_role($2, r.oid, 'MEMBER'),
		       pg_has_role($2, r.oid, 'USAGE'),
		       coalesce(array_agg(m.rolname) FILTER (WHERE m.rolname IS NOT NULL), '{}')
		FROM pg_roles r
		LEFT JOIN pg_auth_members am ON am.roleid = r.oid
		LEFT JOIN pg_roles m ON m.oid = am.member
		WHERE r.rolname = $1
		GROUP BY r.rolcanlogin, r.rolsuper, r.rolbypassrls, r.oid
	`, pgtest.IdentityRole, pgtest.AppRole).Scan(
		&canLogin, &super, &bypassRLS, &appIsMember, &appHasUsage, &members)
	if err != nil {
		t.Fatalf("reading the identity role: %v", err)
	}

	t.Logf("%s: canlogin=%t super=%t bypassrls=%t explicit_members=%v",
		pgtest.IdentityRole, canLogin, super, bypassRLS, members)
	t.Logf("%s may SET ROLE to it: %t (inherits its privileges: %t)", pgtest.AppRole, appIsMember, appHasUsage)

	if canLogin {
		t.Errorf("%s can log in; it is meant to be reachable only from inside its own functions", pgtest.IdentityRole)
	}

	if super {
		t.Errorf("%s is a superuser; it would bypass every policy, not just reach identity data", pgtest.IdentityRole)
	}

	if bypassRLS {
		t.Errorf("%s holds BYPASSRLS; the policies it runs under would be decorative", pgtest.IdentityRole)
	}

	if appIsMember || appHasUsage {
		t.Errorf("%s can act as %s (member=%t usage=%t); it could read the user directory without calling any function",
			pgtest.AppRole, pgtest.IdentityRole, appIsMember, appHasUsage)
	}
}

// TestTheIdentityRoleOwnsNoTables is the counterpart to
// TestTheAppRoleOwnsNothing: a table's owner is exempt from its own policies
// unless the table is FORCEd, and the identity role owning one would be a
// quieter version of granting it BYPASSRLS.
func TestTheIdentityRoleOwnsNoTables(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)

	var owned []string

	rows, err := owner.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind IN ('r', 'v', 'm', 'S')
		  AND pg_get_userbyid(c.relowner) = $1
		ORDER BY c.relname
	`, pgtest.IdentityRole)
	if err != nil {
		t.Fatalf("reading object ownership: %v", err)
	}

	defer rows.Close()

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning owned object: %v", err)
		}

		owned = append(owned, name)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading object ownership: %v", err)
	}

	t.Logf("%s owns %d relations in schema public", pgtest.IdentityRole, len(owned))

	if len(owned) != 0 {
		t.Errorf("%s owns %v", pgtest.IdentityRole, owned)
	}

	// CREATE on the schema is granted for the four statements that hand it
	// ownership of its functions and revoked immediately after. If the revoke
	// were ever dropped, the identity role could add objects to public — new
	// tables, and functions owned by itself — which is how a role that owns
	// nothing quietly starts owning something.
	var mayUse, mayCreate bool

	if err := owner.QueryRow(ctx, `
		SELECT has_schema_privilege($1, 'public', 'USAGE'),
		       has_schema_privilege($1, 'public', 'CREATE')
	`, pgtest.IdentityRole).Scan(&mayUse, &mayCreate); err != nil {
		t.Fatalf("reading schema privileges: %v", err)
	}

	t.Logf("%s on schema public: usage=%t create=%t", pgtest.IdentityRole, mayUse, mayCreate)

	if !mayUse {
		t.Errorf("%s cannot USE schema public; its own functions would fail to resolve their tables", pgtest.IdentityRole)
	}

	if mayCreate {
		t.Errorf("%s holds CREATE on schema public; the migration granted it to reassign ownership and never took it back",
			pgtest.IdentityRole)
	}
}
