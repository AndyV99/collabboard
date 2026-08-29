//go:build integration

package store_test

// The bound on organizations.name, as the database enforces it (issue #67).
//
// internal/auth checks the same rule and is what produces the 400 with a
// sentence naming the field; a constraint violation would surface as a 500. So
// this is the backstop, exactly as `CHECK (length(btrim(name)) > 0)` from
// migration 00002 is the backstop for a blank name.
//
// It is worth having because the column has more than one writer — Register and
// CreateFirstOrganization reach it through provisionOrganization, and #90 adds
// a rename that will not. A constraint makes the bound true of the *data*; a
// check in one function makes it true of one code path.
//
// Written as an integration test because a CHECK constraint is not a claim about
// SQL text, it is a claim about a database.
//
// It inserts as the **superuser**, which is the one place in this package where
// that is the right choice rather than the trap ADR 0001 warns about. A
// superuser bypasses row-level security and nothing else — a CHECK still
// applies — so this isolates the constraint from the policy. Going through the
// tenant path instead would mean an RLS failure and a constraint failure were
// the same red, and the first would hide the second.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestOrganizationsNameIsBoundedByTheSchema(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)

	insert := func(t *testing.T, name string) error {
		t.Helper()

		// A slug that cannot collide with another subtest's, since the unique
		// index on it is not what is under test here.
		slug := "bound-" + strings.ToLower(uuid.NewString())

		_, err := superuser.Exec(ctx,
			`INSERT INTO organizations (name, slug) VALUES ($1, $2)`, name, slug)

		return err
	}

	t.Run("a name at the limit is stored", func(t *testing.T) {
		if err := insert(t, strings.Repeat("a", 200)); err != nil {
			t.Fatalf("inserting a 200-character name: %v", err)
		}
	})

	t.Run("a name one character over is refused", func(t *testing.T) {
		err := insert(t, strings.Repeat("a", 201))
		if err == nil {
			t.Fatal("a 201-character name was accepted; the constraint is not doing anything")
		}

		if !strings.Contains(err.Error(), "organizations_name_length") {
			t.Errorf("error = %v, want a violation of organizations_name_length", err)
		}
	})

	t.Run("the count is characters, not bytes", func(t *testing.T) {
		// 200 U+1F642 is 200 characters and 800 bytes. Postgres `length()`
		// counts characters in a UTF-8 database, which is what makes it agree
		// with Go's utf8.RuneCountInString rather than being four times
		// stricter — and a constraint stricter than the application check would
		// turn a valid name into a 500.
		if err := insert(t, strings.Repeat("\U0001F642", 200)); err != nil {
			t.Fatalf("inserting 200 emoji: %v", err)
		}
	})

	t.Run("whitespace is trimmed before counting, as the blank check already does", func(t *testing.T) {
		if err := insert(t, "   "+strings.Repeat("a", 200)+"   "); err != nil {
			t.Fatalf("inserting a padded 200-character name: %v", err)
		}
	})

	t.Run("the blank check from 00002 still applies", func(t *testing.T) {
		// Adding a constraint must not replace one. Both are named checks on
		// the same column and a migration that dropped the older one would
		// leave a hole nothing else covers.
		if err := insert(t, "   "); err == nil {
			t.Fatal("a blank name was accepted; the constraint from 00002 is gone")
		}
	})
}
