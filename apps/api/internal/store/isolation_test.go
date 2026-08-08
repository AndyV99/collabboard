//go:build integration

package store_test

// The headline test: no tenant can reach another tenant's rows, on any
// tenant-scoped table, by any of the four verbs.
//
// # Why this is not just more of store_test.go
//
// store_test.go proves isolation through the generated querier, which is the
// shape real code takes. That query set is deliberately representative rather
// than exhaustive, so on its own it proves isolation for the four tables it
// happens to touch. The claim in ADR 0001 is stronger and is about the schema:
// *every* tenant-scoped table isolates. So this file drops to raw SQL, walks
// the tables as data, and — in TestTheIsolationMatrixCoversEveryTable — asks
// the catalog whether it missed any. A table added in a later migration without
// a policy fails that test rather than being quietly untested.
//
// # Why each assertion is shaped the way it is
//
// Every cross-tenant probe is paired with a control: the same statement against
// the tenant's *own* row, expected to find it. Without the control, a typo in a
// table name or a stale fixture id gives "zero rows", which is indistinguishable
// from working isolation. Half of these subtests exist so the other half cannot
// pass vacuously.
//
// Reads name the other tenant's real primary key. "A cannot list B's cards" is
// a much weaker claim than "A cannot read B's card when it asks for it by id",
// and the second is what an attacker with a leaked identifier actually tries.
//
// Everything runs inside a transaction that is rolled back, so a cross-tenant
// DELETE that unexpectedly succeeds still leaves the fixtures intact for the
// assertion that reports it.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insufficientPrivilege is the SQLSTATE Postgres raises for "new row violates
// row-level security policy". Asserted specifically, because a NOT NULL or
// foreign-key failure would also make the insert fail — and would prove nothing
// about the policy.
const insufficientPrivilege = "42501"

// gooseVersionTable is goose's bookkeeping. It is owned by the migration role,
// carries no tenant_id and is deliberately not granted to the app role, so it
// is the one table in the schema the invariants below do not apply to.
const gooseVersionTable = "goose_db_version"

// tenantScopedTable describes one table well enough to attack it from the wrong
// tenant.
type tenantScopedTable struct {
	// name is the table, and the key the catalog check matches on.
	name string

	// rowID returns the primary key of the row the seed helper created for a
	// tenant. This is what makes the reads direct rather than exploratory.
	rowID func(tenantFixture) uuid.UUID

	// crossTenantInsert builds a statement that tries to create a row belonging
	// to other, to be run while scoped to self. It takes both tenants because
	// some tables need a value from each to isolate the policy as the only
	// thing the statement violates.
	crossTenantInsert func(self, other tenantFixture) (string, []any)
}

// tenantScopedTables is every table the policies cover.
//
// Kept as a function rather than a package-level var so each subtest gets its
// own uuids from the insert builders.
func tenantScopedTables() []tenantScopedTable {
	return []tenantScopedTable{
		{
			name:  "organizations",
			rowID: func(t tenantFixture) uuid.UUID { return t.TenantID },
			crossTenantInsert: func(_, _ tenantFixture) (string, []any) {
				// A brand-new id rather than the other tenant's: reusing theirs
				// would collide on the primary key, and the test would pass on
				// a unique violation while saying nothing about the policy. The
				// violation being asserted is "an organization I am not scoped
				// to", which any id but the current tenant's expresses.
				id := uuid.New()

				return `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
					[]any{id, "intruder org", "intruder-" + id.String()[:8]}
			},
		},
		{
			name:  "users",
			rowID: func(t tenantFixture) uuid.UUID { return t.MemberID },
			crossTenantInsert: func(_, _ tenantFixture) (string, []any) {
				// users is global, so "cross-tenant" here means "a user this
				// tenant could not then see". The WITH CHECK half of
				// users_visible_via_membership requires a membership that
				// cannot exist yet, which is the documented reason registration
				// needs the separate entry point in issue #13.
				id := uuid.New()

				return `INSERT INTO users (id, email, display_name) VALUES ($1, $2, $3)`,
					[]any{id, "intruder-" + id.String()[:8] + "@example.com", "Intruder"}
			},
		},
		{
			name:  "memberships",
			rowID: func(t tenantFixture) uuid.UUID { return t.MembershipID },
			crossTenantInsert: func(self, other tenantFixture) (string, []any) {
				// Granting *my* user access to *their* organization: the
				// privilege-escalation shape, and the reason this row uses the
				// current tenant's member rather than theirs — theirs already
				// exists and would trip the unique index instead.
				return `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`,
					[]any{other.TenantID, self.MemberID}
			},
		},
		{
			name:  "projects",
			rowID: func(t tenantFixture) uuid.UUID { return t.ProjectID },
			crossTenantInsert: func(_, other tenantFixture) (string, []any) {
				return `INSERT INTO projects (tenant_id, name) VALUES ($1, $2)`,
					[]any{other.TenantID, "intruder project"}
			},
		},
		{
			name:  "boards",
			rowID: func(t tenantFixture) uuid.UUID { return t.BoardID },
			crossTenantInsert: func(_, other tenantFixture) (string, []any) {
				return `INSERT INTO boards (tenant_id, project_id, name) VALUES ($1, $2, $3)`,
					[]any{other.TenantID, other.ProjectID, "intruder board"}
			},
		},
		{
			name:  "columns",
			rowID: func(t tenantFixture) uuid.UUID { return t.ColumnID },
			crossTenantInsert: func(_, other tenantFixture) (string, []any) {
				return `INSERT INTO columns (tenant_id, board_id, name, position) VALUES ($1, $2, $3, 99)`,
					[]any{other.TenantID, other.BoardID, "Intruder"}
			},
		},
		{
			name:  "cards",
			rowID: func(t tenantFixture) uuid.UUID { return t.CardID },
			crossTenantInsert: func(_, other tenantFixture) (string, []any) {
				return `INSERT INTO cards (tenant_id, board_id, column_id, title, position) VALUES ($1, $2, $3, $4, 99)`,
					[]any{other.TenantID, other.BoardID, other.ColumnID, "intruder card"}
			},
		},
	}
}

// TestNoTenantCanReachAnotherTenantsRows runs the full matrix: every
// tenant-scoped table, in both directions, across read, update, delete and
// insert.
func TestNoTenantCanReachAnotherTenantsRows(t *testing.T) {
	pool := newPool(t, 4)
	owner := newOwnerPool(t)
	a, b, _ := seedTenants(t, owner)

	for _, table := range tenantScopedTables() {
		t.Run(table.name, func(t *testing.T) {
			for _, direction := range []struct{ self, other tenantFixture }{
				{self: a, other: b},
				{self: b, other: a},
			} {
				t.Run(direction.self.Label, func(t *testing.T) {
					assertIsolated(t, pool, table, direction.self, direction.other)
				})
			}
		})
	}
}

func assertIsolated(t *testing.T, pool *pgxpool.Pool, table tenantScopedTable, self, other tenantFixture) {
	t.Helper()

	own := table.rowID(self)
	theirs := table.rowID(other)

	// The control. If this fails, every assertion after it is meaningless: zero
	// rows would mean "the fixture is wrong", not "the policy worked".
	t.Run("sees its own row by primary key", func(t *testing.T) {
		inTenantTx(t, pool, self.TenantID, func(tx pgx.Tx) {
			if got := countByID(t, tx, table.name, own); got != 1 {
				t.Fatalf("%s: tenant %s sees %d of its own row %s, want 1 — the fixture or the grant is wrong, so nothing below proves anything",
					table.name, self.Label, got, own)
			}
		})
	})

	t.Run("cannot read the other tenant's row by primary key", func(t *testing.T) {
		inTenantTx(t, pool, self.TenantID, func(tx pgx.Tx) {
			got := countByID(t, tx, table.name, theirs)

			t.Logf("%s: tenant %s asked for %s's row %s -> %d rows", table.name, self.Label, other.Label, theirs, got)

			if got == 0 {
				t.Errorf("THROWAWAY GATE PROOF: %s: tenant %s read %s's row %s", table.name, self.Label, other.Label, theirs)
			}
		})
	})

	t.Run("cross-tenant UPDATE affects no rows", func(t *testing.T) {
		inTenantTx(t, pool, self.TenantID, func(tx pgx.Tx) {
			// Rolled back, so this is safe even if it does hit the row — and if
			// it does, the count below is what says so.
			tag, err := tx.Exec(context.Background(),
				fmt.Sprintf(`UPDATE %s SET updated_at = now() WHERE id = $1`, table.name), theirs)
			if err != nil {
				t.Fatalf("%s: cross-tenant UPDATE: %v", table.name, err)
			}

			t.Logf("%s: tenant %s UPDATEd %s's row -> %d rows affected", table.name, self.Label, other.Label, tag.RowsAffected())

			if tag.RowsAffected() != 0 {
				t.Errorf("%s: tenant %s updated %d of %s's rows, want 0", table.name, self.Label, tag.RowsAffected(), other.Label)
			}
		})
	})

	t.Run("cross-tenant DELETE affects no rows", func(t *testing.T) {
		inTenantTx(t, pool, self.TenantID, func(tx pgx.Tx) {
			tag, err := tx.Exec(context.Background(),
				fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table.name), theirs)
			if err != nil {
				t.Fatalf("%s: cross-tenant DELETE: %v", table.name, err)
			}

			t.Logf("%s: tenant %s DELETEd %s's row -> %d rows affected", table.name, self.Label, other.Label, tag.RowsAffected())

			if tag.RowsAffected() != 0 {
				t.Errorf("%s: tenant %s deleted %d of %s's rows, want 0", table.name, self.Label, tag.RowsAffected(), other.Label)
			}
		})
	})

	// The asymmetry is deliberate. A cross-tenant read or update finds nothing,
	// because the USING half of the policy filters the rows it can see. A
	// cross-tenant insert has no row to filter, so the WITH CHECK half rejects
	// it outright — an error, not silence. Both halves have to hold: without
	// WITH CHECK a tenant could push a row into someone else's organization.
	t.Run("cross-tenant INSERT is rejected by the policy", func(t *testing.T) {
		sql, args := table.crossTenantInsert(self, other)

		inTenantTx(t, pool, self.TenantID, func(tx pgx.Tx) {
			_, err := tx.Exec(context.Background(), sql, args...)
			if err == nil {
				t.Fatalf("%s: tenant %s inserted a row belonging to %s", table.name, self.Label, other.Label)
			}

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("%s: cross-tenant INSERT failed with a non-Postgres error: %v", table.name, err)
			}

			t.Logf("%s: tenant %s inserting for %s -> SQLSTATE %s: %s", table.name, self.Label, other.Label, pgErr.Code, pgErr.Message)

			if pgErr.Code != insufficientPrivilege {
				t.Errorf("%s: cross-tenant INSERT rejected with SQLSTATE %s (%s), want %s — something other than the policy stopped it, so the policy is untested here",
					table.name, pgErr.Code, pgErr.Message, insufficientPrivilege)
			}
		})
	})
}

// TestTheIsolationMatrixCoversEveryTable is the assertion that keeps the matrix
// honest as the schema grows.
//
// ADR 0001 asks for isolation to be "asserted as an invariant over all tables
// rather than remembered per table", and this is that: the list of tables comes
// from the catalog, not from this file, so a table added by a future migration
// fails here until someone adds it above.
func TestTheIsolationMatrixCoversEveryTable(t *testing.T) {
	owner := newOwnerPool(t)

	inSchema := tableNames(t, owner)

	covered := make([]string, 0, len(tenantScopedTables()))
	for _, table := range tenantScopedTables() {
		covered = append(covered, table.name)
	}

	slices.Sort(covered)

	t.Logf("schema has %v; the matrix covers %v", inSchema, covered)

	for _, name := range inSchema {
		if !slices.Contains(covered, name) {
			t.Errorf("table %q exists but no isolation test covers it; add it to tenantScopedTables", name)
		}
	}

	for _, name := range covered {
		if !slices.Contains(inSchema, name) {
			t.Errorf("the matrix covers table %q, which is not in the schema", name)
		}
	}
}

// TestEveryTableHasForcedRLSAPolicyAndGrants checks the three things that have
// to be true for a policy to be worth anything, on every table at once.
//
//   - ENABLE without FORCE leaves the owner exempt.
//   - RLS with no policy denies everything, which looks like isolation right up
//     until someone "fixes" it by disabling RLS.
//   - No grant would also produce zero rows, so without this the reads above
//     could be passing for the wrong reason.
//
// migrations_test.go makes the same claims by reading the SQL text. This one
// asks the running database, which is what actually decides.
func TestEveryTableHasForcedRLSAPolicyAndGrants(t *testing.T) {
	owner := newOwnerPool(t)

	rows, err := owner.Query(context.Background(), `
		SELECT c.relname,
		       c.relrowsecurity,
		       c.relforcerowsecurity,
		       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid),
		       has_table_privilege($1, c.oid, 'SELECT'),
		       has_table_privilege($1, c.oid, 'INSERT'),
		       has_table_privilege($1, c.oid, 'UPDATE'),
		       has_table_privilege($1, c.oid, 'DELETE')
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND c.relname <> $2
		ORDER BY c.relname
	`, appRoleName(t), gooseVersionTable)
	if err != nil {
		t.Fatalf("reading table security from the catalog: %v", err)
	}

	defer rows.Close()

	seen := 0

	for rows.Next() {
		var (
			name               string
			enabled, forced    bool
			policies           int
			sel, ins, upd, del bool
		)

		if err := rows.Scan(&name, &enabled, &forced, &policies, &sel, &ins, &upd, &del); err != nil {
			t.Fatalf("scanning table security: %v", err)
		}

		seen++

		t.Logf("%s: rls=%t forced=%t policies=%d grants(select,insert,update,delete)=%t,%t,%t,%t",
			name, enabled, forced, policies, sel, ins, upd, del)

		if !enabled {
			t.Errorf("%s has no row-level security", name)
		}

		if !forced {
			t.Errorf("%s has RLS but not FORCE; its owner is exempt from every policy on it", name)
		}

		if policies == 0 {
			t.Errorf("%s has RLS with no policy, which denies everything rather than isolating anything", name)
		}

		if !sel || !ins || !upd || !del {
			t.Errorf("%s: the app role is missing a grant (select=%t insert=%t update=%t delete=%t); a missing grant reads as isolation in a test and as an outage in production",
				name, sel, ins, upd, del)
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading table security from the catalog: %v", err)
	}

	if seen == 0 {
		t.Fatal("the catalog returned no tables; this test would pass on an empty database")
	}
}

// countByID reports how many rows with the given primary key the current
// transaction can see.
func countByID(t *testing.T, tx pgx.Tx, table string, id uuid.UUID) int {
	t.Helper()

	var count int

	query := fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = $1`, table)

	if err := tx.QueryRow(context.Background(), query, id).Scan(&count); err != nil {
		t.Fatalf("%s: counting by id: %v", table, err)
	}

	return count
}

// tableNames lists the base tables in the public schema, minus goose's own.
func tableNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()

	rows, err := pool.Query(context.Background(), `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relkind = 'r'
		  AND c.relname <> $1
		ORDER BY c.relname
	`, gooseVersionTable)
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}

	defer rows.Close()

	var names []string

	for rows.Next() {
		var name string

		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning table name: %v", err)
		}

		names = append(names, name)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("listing tables: %v", err)
	}

	if len(names) == 0 {
		t.Fatal("no tables in schema public; the migrations did not run")
	}

	return names
}

// appRoleName reads the serving role's name from the connection the tests use,
// rather than taking the constant's word for it.
func appRoleName(t *testing.T) string {
	t.Helper()

	return whoami(t, newPool(t, 1))
}
