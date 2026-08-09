package migrations_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/migrations"
)

// The real proof that isolation holds is an integration test against a live
// Postgres — internal/store/isolation_test.go, behind the `integration` build
// tag. These tests are the cheap complement: they catch the specific mistake
// the ADR calls out — a table added later without RLS, or with RLS enabled but
// not FORCEd — at the point the migration is written, with no database and no
// container.

var (
	// The name is captured with its schema qualifier when it has one, because
	// the ALTER TABLE and CREATE POLICY statements checked below have to name
	// the table the same way the CREATE did. Without the optional `\.[a-z_]+`
	// group, `CREATE TABLE auth.user_credentials` would be read as a table
	// called "auth" and every assertion about it would look for statements
	// that could not exist.
	createTableRe = regexp.MustCompile(`(?im)^\s*CREATE TABLE\s+(?:IF NOT EXISTS\s+)?([a-z_]+(?:\.[a-z_]+)?)`)
	gooseUpRe     = regexp.MustCompile(`(?m)^--\s*\+goose Up\s*$`)
	gooseDownRe   = regexp.MustCompile(`(?m)^--\s*\+goose Down\s*$`)
)

// notReachableByTheAppRole is the explicit, deliberately short list of tables
// the serving role is *meant* to have no grant on.
//
// The default is the opposite: a table with RLS and no grant to
// collabboard_app is unreadable, which reads as isolation in a test and as an
// outage in production, so the check below insists on the grant. This list is
// where a table opts out, and every entry has to earn it in review.
//
// auth.user_credentials earns it because the whole point of migration 00005 is
// that the serving role cannot reach password material: it holds no USAGE on
// schema auth, so it cannot name the table at all, and only the three
// SECURITY DEFINER functions owned by collabboard_credentials ever touch it.
// See docs/adr/0003-password-verifier-storage.md.
var notReachableByTheAppRole = map[string]string{
	"auth.user_credentials": "credential storage; collabboard_app holds no USAGE on schema auth by design (ADR 0003)",
}

func TestEveryCreatedTableIsProtectedByForcedRLS(t *testing.T) {
	forEachMigration(t, func(t *testing.T, name, body string) {
		t.Helper()

		tables := createTableRe.FindAllStringSubmatch(body, -1)
		if len(tables) == 0 {
			t.Skipf("%s creates no tables", name)
		}

		for _, match := range tables {
			table := match[1]

			for _, want := range []string{
				"ALTER TABLE " + table + " ENABLE ROW LEVEL SECURITY;",
				"ALTER TABLE " + table + " FORCE ROW LEVEL SECURITY;",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("%s creates table %q but never runs %q", name, table, want)
				}
			}

			if !regexp.MustCompile(`(?is)CREATE POLICY\s+\S+\s+ON\s+` + regexp.QuoteMeta(table) + `\b`).MatchString(body) {
				t.Errorf("%s creates table %q with no policy: RLS with no policy denies everything", name, table)
			}

			// Table-aware rather than a bare substring search over the file:
			// migration 00005 grants EXECUTE on functions to collabboard_app
			// while deliberately granting it nothing on the table it creates,
			// and a file-level search would call that a pass.
			// The table is matched anywhere inside the statement rather than
			// immediately after ON, because a grant may name several tables at
			// once — 00002 grants on `organizations, users, memberships` in one
			// statement. [^;]* keeps the match inside a single statement, which
			// is what stops one file's unrelated grant from covering another
			// table in the same file.
			grantsToApp := regexp.MustCompile(
				`(?is)GRANT\s[^;]*\b` + regexp.QuoteMeta(table) + `\b[^;]*\bTO\s+collabboard_app\b`,
			).MatchString(body)

			why, exempt := notReachableByTheAppRole[table]

			switch {
			case exempt && grantsToApp:
				t.Errorf("%s grants collabboard_app a privilege on %q, which is listed as unreachable by the app role (%s)", name, table, why)
			case exempt:
				t.Logf("%s creates %q with no grant to collabboard_app, deliberately: %s", name, table, why)
			case !grantsToApp:
				t.Errorf("%s creates table %q but grants nothing on it to collabboard_app; a missing grant reads as isolation in a test and as an outage in production", name, table)
			}
		}
	})
}

// A migration with no Down is a migration that cannot be rolled back in an
// incident, which is when rollback is actually needed.
func TestEveryMigrationHasUpAndDown(t *testing.T) {
	forEachMigration(t, func(t *testing.T, name, body string) {
		t.Helper()

		if !gooseUpRe.MatchString(body) {
			t.Errorf("%s has no `-- +goose Up` annotation", name)
		}

		if !gooseDownRe.MatchString(body) {
			t.Errorf("%s has no `-- +goose Down` annotation", name)
		}
	})
}

// roleStatementRe captures one whole CREATE ROLE or ALTER ROLE statement.
//
// The checks below used to scan the entire file, which stopped working the
// moment a migration had to *verify* an attribute rather than set it: issue #14
// replaced `ALTER ROLE ... NOBYPASSRLS` with a DO block that reads
// pg_roles.rolbypassrls and raises, and a whole-file scan reads that column
// name as the migration granting BYPASSRLS. Scoping to role statements is not a
// weakening — role attributes can only be conferred by CREATE ROLE or ALTER
// ROLE, so nothing else could confer one. [^;] keeps a match inside a single
// statement.
var roleStatementRe = regexp.MustCompile(`(?is)\b(CREATE|ALTER)\s+ROLE\s[^;]*`)

// Guards against a role being handed privileges that would make RLS decorative,
// however the migration files are edited later.
func TestMigrationsNeverGrantARoleAWayOut(t *testing.T) {
	// CREATEROLE stays on the list even though the schema owner holds it: the
	// owner gets it from apps/api/scripts/provision/bootstrap-owner.sql, run by
	// a privileged identity out of band. A migration conferring it would mean a
	// role the migrations themselves created could create more.
	banned := []string{"BYPASSRLS", "SUPERUSER", "CREATEROLE", "CREATEDB", "REPLICATION"}

	forEachMigration(t, func(t *testing.T, name, body string) {
		t.Helper()

		for _, statement := range roleStatements(body) {
			for _, word := range banned {
				// Only the negated form is legitimate, so strip those first and
				// see whether the bare attribute is still conferred.
				if strings.Contains(strings.ReplaceAll(statement, "NO"+word, ""), word) {
					t.Errorf("%s confers %s in %q; no role this schema creates may hold any of %v",
						name, word, collapse(statement), banned)
				}
			}
		}
	})
}

// attributesOnlyAHolderCanSet are the attributes PostgreSQL refuses to let a
// role change unless it holds them itself — asserting the negative counts as
// changing it. Verified against PostgreSQL 16.14; the error is
// "permission denied to alter role / Only roles with the X attribute may change
// the X attribute".
//
// CREATEROLE is not in the list: a CREATEROLE role may clear CREATEROLE, which
// is why 00001, 00004 and 00005 can still say NOCREATEROLE outright.
var attributesOnlyAHolderCanSet = []string{"NOSUPERUSER", "NOBYPASSRLS", "NOCREATEDB", "NOREPLICATION"}

// TestMigrationsOnlyAlterAttributesTheOwnerCanSet is the regression guard for
// the specific thing issue #14 found: the migration chain could not be applied
// by the non-superuser owner it claimed to target, because 00001, 00004 and
// 00005 each asserted attributes only a superuser may assert.
//
// CREATE ROLE is deliberately exempt. PostgreSQL only checks these attributes
// when they are being *changed*, and a role being created has them off already,
// so `CREATE ROLE x NOBYPASSRLS` is legal for any creator — which is why the
// migrations state them there and check them afterwards.
func TestMigrationsOnlyAlterAttributesTheOwnerCanSet(t *testing.T) {
	forEachMigration(t, func(t *testing.T, name, body string) {
		t.Helper()

		for _, statement := range roleStatements(body) {
			if !strings.HasPrefix(statement, "ALTER") {
				continue
			}

			for _, attribute := range attributesOnlyAHolderCanSet {
				if strings.Contains(statement, attribute) {
					t.Errorf("%s says %s in %q; only a role that itself holds the attribute may set it, so this makes the chain unrunnable as the non-superuser owner. Verify it in a DO block instead — see 00001.",
						name, attribute, collapse(statement))
				}
			}
		}
	})
}

// roleStatements returns every CREATE ROLE / ALTER ROLE statement in a
// migration, upper-cased and with comments removed.
//
// Comments discuss these attributes by name, which is the point of the
// comments; only executable SQL is checked.
func roleStatements(body string) []string {
	return roleStatementRe.FindAllString(strings.ToUpper(stripSQLComments(body)), -1)
}

// collapse renders a statement on one line, so a multi-line ALTER ROLE reads as
// one thing in a failure message.
func collapse(statement string) string {
	return strings.Join(strings.Fields(statement), " ")
}

// stripSQLComments removes `--` line comments. Crude on purpose: it does not
// understand string literals, which is fine because it is only used to keep
// prose out of a keyword scan.
func stripSQLComments(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))

	for _, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}

		out = append(out, line)
	}

	return strings.Join(out, "\n")
}

func forEachMigration(t *testing.T, check func(t *testing.T, name, body string)) {
	t.Helper()

	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("globbing embedded migrations: %v", err)
	}

	if len(entries) == 0 {
		t.Fatal("no migrations embedded; the go:embed directive is not matching")
	}

	for _, name := range entries {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}

		t.Run(name, func(t *testing.T) {
			check(t, name, string(body))
		})
	}
}
