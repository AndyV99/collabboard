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
	createTableRe = regexp.MustCompile(`(?im)^\s*CREATE TABLE\s+(?:IF NOT EXISTS\s+)?([a-z_]+)`)
	gooseUpRe     = regexp.MustCompile(`(?m)^--\s*\+goose Up\s*$`)
	gooseDownRe   = regexp.MustCompile(`(?m)^--\s*\+goose Down\s*$`)
)

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

			if !regexp.MustCompile(`(?is)CREATE POLICY\s+\S+\s+ON\s+` + table + `\b`).MatchString(body) {
				t.Errorf("%s creates table %q with no policy: RLS with no policy denies everything", name, table)
			}

			if !strings.Contains(body, "TO collabboard_app") {
				t.Errorf("%s creates table %q but grants nothing to collabboard_app", name, table)
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

// Guards against the app role being handed privileges that would make RLS
// decorative, however the migration files are edited later.
func TestMigrationsNeverGrantTheAppRoleAWayOut(t *testing.T) {
	banned := []string{"BYPASSRLS", "SUPERUSER", "CREATEROLE", "CREATEDB", "REPLICATION"}

	forEachMigration(t, func(t *testing.T, name, body string) {
		t.Helper()

		// Comments discuss these attributes by name, which is the point of the
		// comments; only executable SQL is checked.
		upper := strings.ToUpper(stripSQLComments(body))

		for _, word := range banned {
			// Only the negated form is legitimate, so strip those first and see
			// whether the bare attribute is still granted anywhere.
			if strings.Contains(strings.ReplaceAll(upper, "NO"+word, ""), word) {
				t.Errorf("%s grants %s to a role; the app role must have none of %v", name, word, banned)
			}
		}
	})
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
