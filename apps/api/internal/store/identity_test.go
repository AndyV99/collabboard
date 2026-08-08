//go:build integration

package store_test

// The test that makes every other test in this package mean something.
//
// Row-level security is bypassed by superusers, by roles holding BYPASSRLS, and
// by a table's owner unless FORCE ROW LEVEL SECURITY is set. A suite that
// connects as any of those passes every isolation assertion in the file next to
// this one while proving nothing at all — the policies are still there, they
// simply do not apply. ADR 0001 names this as the failure mode the whole
// integration harness exists to prevent, so it is asserted here rather than
// left to a comment claiming the DSN is the right one.

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
)

// TestTheSuiteConnectsAsTheAppRole asserts, from inside the connection the tests
// actually use, that the identity on the other end is the serving role and that
// it holds none of the three exemptions.
func TestTheSuiteConnectsAsTheAppRole(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 1)

	var (
		currentUser string
		sessionUser string
		super       bool
		bypassRLS   bool
	)

	// One statement, so the four facts describe the same session. session_user
	// as well as current_user: a SECURITY DEFINER function or a SET ROLE would
	// make them disagree, and the identity that matters for RLS is the current
	// one.
	err := pool.QueryRow(ctx, `
		SELECT current_user,
		       session_user,
		       r.rolsuper,
		       r.rolbypassrls
		FROM pg_roles r
		WHERE r.rolname = current_user
	`).Scan(&currentUser, &sessionUser, &super, &bypassRLS)
	if err != nil {
		t.Fatalf("reading the connection's identity: %v", err)
	}

	t.Logf("connected as current_user=%s session_user=%s rolsuper=%t rolbypassrls=%t",
		currentUser, sessionUser, super, bypassRLS)

	if currentUser != pgtest.AppRole {
		t.Errorf("current_user = %q, want %q: these tests are not exercising the policies at all",
			currentUser, pgtest.AppRole)
	}

	if sessionUser != pgtest.AppRole {
		t.Errorf("session_user = %q, want %q", sessionUser, pgtest.AppRole)
	}

	if super {
		t.Errorf("%s is a superuser; row-level security does not apply to it", currentUser)
	}

	if bypassRLS {
		t.Errorf("%s holds BYPASSRLS; row-level security does not apply to it", currentUser)
	}
}

// TestTheAppRoleInheritsNoExemption closes the gap the previous test leaves:
// rolsuper and rolbypassrls describe the role itself, but a role also acts with
// the attributes of every role it is a member of. GRANT pg_write_all_data TO
// collabboard_app would leave both flags false and still undo the isolation.
//
// pg_has_role walks that graph, so this holds however the role is granted.
func TestTheAppRoleInheritsNoExemption(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 1)

	rows, err := pool.Query(ctx, `
		SELECT r.rolname, r.rolsuper, r.rolbypassrls
		FROM pg_roles r
		WHERE pg_has_role(current_user, r.oid, 'USAGE')
		  AND (r.rolsuper OR r.rolbypassrls)
		ORDER BY r.rolname
	`)
	if err != nil {
		t.Fatalf("reading inherited role attributes: %v", err)
	}

	defer rows.Close()

	for rows.Next() {
		var (
			name      string
			super     bool
			bypassRLS bool
		)

		if err := rows.Scan(&name, &super, &bypassRLS); err != nil {
			t.Fatalf("scanning inherited role: %v", err)
		}

		t.Errorf("%s inherits role %q (rolsuper=%t, rolbypassrls=%t), which exempts it from every policy",
			pgtest.AppRole, name, super, bypassRLS)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading inherited role attributes: %v", err)
	}
}

// TestTheAppRoleOwnsNothing is the third exemption, and the least obvious: a
// table's owner is exempt from its own policies unless the table is FORCEd. The
// migrations do force every table, so this is belt and braces — but it is the
// belt that keeps working if someone later adds a table and forgets FORCE, and
// it is one query.
func TestTheAppRoleOwnsNothing(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 1)

	var owned []string

	rows, err := pool.Query(ctx, `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind IN ('r', 'v', 'm', 'S')
		  AND pg_get_userbyid(c.relowner) = current_user
		ORDER BY c.relname
	`)
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

	t.Logf("%s owns %d objects in schema public", pgtest.AppRole, len(owned))

	if len(owned) != 0 {
		t.Errorf("%s owns %v; an owner is exempt from its own policies unless the table is FORCEd", pgtest.AppRole, owned)
	}
}

// TestHarnessIdentitiesAreDistinct guards the harness itself. If any two of the
// three DSNs ever resolved to the same role — a copy-paste in pgtest, an
// environment override — the seed path, the migration path and the path under
// test would collapse into one identity and the suite would go quietly vacuous.
//
// The failure this is really watching for is the one issue #14 fixed: for five
// migrations the schema owner and the bootstrap superuser were the same role,
// and nothing said so out loud.
func TestHarnessIdentitiesAreDistinct(t *testing.T) {
	identities := map[string]string{
		"app":         whoami(t, newPool(t, 1)),
		"superuser":   whoami(t, newSuperuserPool(t)),
		"schemaOwner": whoami(t, newSchemaOwnerPool(t)),
	}

	t.Logf("app=%s, superuser=%s, schema owner=%s",
		identities["app"], identities["superuser"], identities["schemaOwner"])

	want := map[string]string{
		"app":         pgtest.AppRole,
		"superuser":   pgtest.SuperuserRole,
		"schemaOwner": pgtest.SchemaOwnerRole,
	}

	for pool, wantRole := range want {
		if got := identities[pool]; got != wantRole {
			t.Errorf("%s pool connects as %q, want %q", pool, got, wantRole)
		}
	}

	seen := make(map[string]string, len(identities))

	for pool, role := range identities {
		if other, dup := seen[role]; dup {
			t.Fatalf("the %s and %s pools both connect as %q; two of the harness's identities are the same role",
				other, pool, role)
		}

		seen[role] = pool
	}
}

func whoami(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	var user string

	if err := pool.QueryRow(context.Background(), `SELECT current_user`).Scan(&user); err != nil {
		t.Fatalf("reading current_user: %v", err)
	}

	return user
}
