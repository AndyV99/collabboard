//go:build integration

package store_test

// The credential half of the pre-tenant path (issue #8), and the claim that
// matters about it: adding a password did not widen the door #13 built.
//
// pretenant_narrow_test.go proves collabboard_identity cannot reach
// tenant-scoped data. This file proves the two things #8 could have broken:
//
//   1. collabboard_identity cannot reach *credential* data either — the
//      identity functions' owner has no USAGE on schema auth and no privilege
//      on auth.user_credentials. So the four functions from 00004 are exactly
//      as capable as they were before this migration.
//   2. collabboard_credentials, the role that can reach credential data, has no
//      privilege of any kind in schema public. It cannot read an email, a
//      display name, a membership or a card. It is strictly narrower than the
//      identity role, not an extension of it.
//
// Both are asserted from the catalog *and* at runtime, by building the exact
// hostile thing each one forbids — a SECURITY DEFINER function owned by one
// role that reaches for the other's data — and calling it as the serving role.
// That is the same technique pretenant_narrow_test.go uses, and it exists for
// the same reason: a catalog assertion describes the grants, and a live probe
// proves what the grants mean.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
)

// credentialsTable is the one table collabboard_credentials may touch, and the
// one table collabboard_identity and collabboard_app may not.
const credentialsTable = "user_credentials"

// tablePrivileges are the verbs asked about at table level. DELETE and TRUNCATE
// exist only at table level, which is why the column list below is shorter.
var tablePrivileges = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES"}

// columnPrivileges are the verbs that can also be granted per column. A
// column-level grant leaves has_table_privilege false while still exposing
// data — which is exactly the shape both pre-tenant roles' own grants take, so
// it is not a hypothetical.
var columnPrivileges = []string{"SELECT", "INSERT", "UPDATE", "REFERENCES"}

// TestOnlyTheCredentialsRoleCanReachCredentialStorage is the headline.
//
// The serving role and the identity role are checked together and identically,
// because the interesting claim is that neither of them is special: whatever
// #8 granted, it granted to a third role, and these two are where they were.
func TestOnlyTheCredentialsRoleCanReachCredentialStorage(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)

	qualified := pgtest.AuthSchema + "." + credentialsTable

	for _, role := range []string{pgtest.AppRole, pgtest.IdentityRole} {
		t.Run(role, func(t *testing.T) {
			var mayUseSchema bool

			if err := superuser.QueryRow(ctx,
				`SELECT has_schema_privilege($1, $2, 'USAGE')`,
				role, pgtest.AuthSchema).Scan(&mayUseSchema); err != nil {
				t.Fatalf("asking the catalog about USAGE on schema %s: %v", pgtest.AuthSchema, err)
			}

			t.Logf("%s on schema %s: usage=%t", role, pgtest.AuthSchema, mayUseSchema)

			// The schema boundary is the strong one. Row-level security filters
			// rows out of a table you can see; no USAGE means the table does
			// not resolve at all, so there is nothing to write a policy about.
			if mayUseSchema {
				t.Errorf("%s holds USAGE on schema %s; it can name the credential table, and only the grants below stand between it and the data",
					role, pgtest.AuthSchema)
			}

			assertNoPrivileges(ctx, t, superuser, role, qualified)
		})
	}

	// And the control: the role that is *supposed* to reach it, does. Without
	// this the assertions above would also pass if migration 00005 had granted
	// nobody anything, and login would be broken rather than secure.
	t.Run(pgtest.CredentialsRole+" (control)", func(t *testing.T) {
		var mayUseSchema, maySelect, mayInsert bool

		if err := superuser.QueryRow(ctx, `
			SELECT has_schema_privilege($1, $2, 'USAGE'),
			       has_column_privilege($1, $3, 'verifier', 'SELECT'),
			       has_column_privilege($1, $3, 'verifier', 'INSERT')
		`, pgtest.CredentialsRole, pgtest.AuthSchema, qualified).Scan(&mayUseSchema, &maySelect, &mayInsert); err != nil {
			t.Fatalf("reading the credentials role's grants: %v", err)
		}

		t.Logf("%s: schema usage=%t verifier select=%t insert=%t",
			pgtest.CredentialsRole, mayUseSchema, maySelect, mayInsert)

		if !mayUseSchema || !maySelect || !mayInsert {
			t.Errorf("%s cannot read or write the verifier (usage=%t select=%t insert=%t); login and registration are broken, and the assertions above prove nothing",
				pgtest.CredentialsRole, mayUseSchema, maySelect, mayInsert)
		}
	})
}

// TestTheCredentialsRoleCannotChangeOrRemoveACredential is the other half of
// "registration only".
//
// The function has no ON CONFLICT clause, but a function body is a line of SQL
// someone can edit. The grant is not: with no UPDATE and no DELETE privilege on
// any column, a body that tried would fail at execution regardless of what it
// says. Password change and password reset therefore cannot be added by editing
// a function — they need a migration, which is the review point.
func TestTheCredentialsRoleCannotChangeOrRemoveACredential(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)
	qualified := pgtest.AuthSchema + "." + credentialsTable

	for _, verb := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
		var atTable bool

		if err := superuser.QueryRow(ctx,
			`SELECT has_table_privilege($1, $2, $3)`,
			pgtest.CredentialsRole, qualified, verb).Scan(&atTable); err != nil {
			t.Fatalf("asking the catalog about %s: %v", verb, err)
		}

		t.Logf("%s on %s: %t", verb, qualified, atTable)

		if atTable {
			t.Errorf("%s holds %s on %s; this path can change or remove a credential, which it is not meant to be able to do",
				pgtest.CredentialsRole, verb, qualified)
		}
	}

	var atColumn bool

	if err := superuser.QueryRow(ctx,
		`SELECT has_any_column_privilege($1, $2, 'UPDATE')`,
		pgtest.CredentialsRole, qualified).Scan(&atColumn); err != nil {
		t.Fatalf("asking the catalog about column UPDATE: %v", err)
	}

	t.Logf("UPDATE on any column of %s: %t", qualified, atColumn)

	if atColumn {
		t.Errorf("%s holds UPDATE on a column of %s", pgtest.CredentialsRole, qualified)
	}
}

// TestTheCredentialsRoleCannotTouchAnythingInPublic is the mirror of
// TestTheIdentityRoleCannotTouchAnyTenantScopedTable, and it is stronger: the
// identity role may see three tables in public, the credentials role may see
// none.
//
// The table list comes from the catalog, so a table added by a future migration
// is covered the moment it exists.
func TestTheCredentialsRoleCannotTouchAnythingInPublic(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)

	tables := tableNames(t, superuser)

	for _, table := range tables {
		assertNoPrivileges(ctx, t, superuser, pgtest.CredentialsRole, table)
	}

	t.Logf("checked %d tables in schema public against %s: %v", len(tables), pgtest.CredentialsRole, tables)
}

// assertNoPrivileges asserts that role holds nothing at all on table, at table
// level or on any single column.
func assertNoPrivileges(ctx context.Context, t *testing.T, superuser *pgxpool.Pool, role, table string) {
	t.Helper()

	for _, verb := range tablePrivileges {
		var atTable bool

		if err := superuser.QueryRow(ctx,
			`SELECT has_table_privilege($1, $2, $3)`, role, table, verb).Scan(&atTable); err != nil {
			t.Fatalf("asking the catalog about %s on %s for %s: %v", verb, table, role, err)
		}

		if atTable {
			t.Errorf("%s holds %s on %s", role, verb, table)
		}
	}

	for _, verb := range columnPrivileges {
		var atColumn bool

		if err := superuser.QueryRow(ctx,
			`SELECT has_any_column_privilege($1, $2, $3)`, role, table, verb).Scan(&atColumn); err != nil {
			t.Fatalf("asking the catalog about column %s on %s for %s: %v", verb, table, role, err)
		}

		if atColumn {
			t.Errorf("%s holds %s on a column of %s; a column grant leaves the table-level answer false while still exposing data",
				role, verb, table)
		}
	}
}

// TestNeitherPreTenantRoleCanReachTheOthersDataFromInsideItsOwnFunction is the
// runtime version of the two catalog tests above, and the one worth reading.
//
// It builds the exact thing this split is afraid of, twice: a SECURITY DEFINER
// function owned by collabboard_identity that reads the credential table, and
// one owned by collabboard_credentials that reads the user directory. Both are
// granted to the serving role and called. Postgres refuses both.
//
// So the separation is not "nobody would write that function". It is that the
// function does not work if they do.
func TestNeitherPreTenantRoleCanReachTheOthersDataFromInsideItsOwnFunction(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)
	app := newPool(t, 1)

	for _, probe := range []struct {
		name   string
		role   string
		target string
	}{
		{
			name:   "identity role reading credential storage",
			role:   pgtest.IdentityRole,
			target: pgtest.AuthSchema + "." + credentialsTable,
		},
		{
			name:   "credentials role reading the user directory",
			role:   pgtest.CredentialsRole,
			target: "public.users",
		},
		{
			name:   "credentials role reading a tenant-scoped table",
			role:   pgtest.CredentialsRole,
			target: "public.cards",
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			fn := "crossover_probe_" + uuid.NewString()[:8]

			create := fmt.Sprintf(`
				CREATE FUNCTION public.%s() RETURNS bigint
				LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, public
				AS 'SELECT count(*) FROM %s';
				ALTER FUNCTION public.%s() OWNER TO %s;
				GRANT EXECUTE ON FUNCTION public.%s() TO %s;
			`, fn, probe.target, fn, probe.role, fn, pgtest.AppRole)

			if _, err := superuser.Exec(ctx, create); err != nil {
				t.Fatalf("creating the probe function: %v", err)
			}

			t.Cleanup(func() {
				if _, derr := superuser.Exec(ctx, fmt.Sprintf(`DROP FUNCTION IF EXISTS public.%s()`, fn)); derr != nil {
					t.Errorf("dropping the probe function: %v", derr)
				}
			})

			var count int64

			err := app.QueryRow(ctx, fmt.Sprintf(`SELECT public.%s()`, fn)).Scan(&count)
			if err == nil {
				t.Fatalf("a SECURITY DEFINER function owned by %s counted %d rows in %s; the two pre-tenant roles are not separated",
					probe.role, count, probe.target)
			}

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("the probe failed with a non-Postgres error: %v", err)
			}

			t.Logf("SECURITY DEFINER count(*) on %s as %s -> SQLSTATE %s: %s",
				probe.target, probe.role, pgErr.Code, pgErr.Message)

			// 42501 is the missing privilege; 3F000 is "schema does not exist",
			// which is what no USAGE on `auth` looks like from inside a
			// function whose search_path does not include it. Either is the
			// boundary doing its job; anything else means something unrelated
			// stopped the probe and the claim is untested.
			if pgErr.Code != insufficientPrivilege && pgErr.Code != invalidSchemaName {
				t.Errorf("the probe on %s was rejected with SQLSTATE %s (%s), want %s or %s",
					probe.target, pgErr.Code, pgErr.Message, insufficientPrivilege, invalidSchemaName)
			}
		})
	}
}

// invalidSchemaName is what a reference to a schema the current role cannot use
// looks like when the reference is qualified.
const invalidSchemaName = "3F000"

// TestTheServingRoleCannotAssumeTheCredentialsRole closes the same door
// pretenant_narrow_test.go closes for the identity role. If collabboard_app
// could SET ROLE to collabboard_credentials, every assertion in this file would
// be decorative — it could read verifiers with a plain SELECT.
func TestTheServingRoleCannotAssumeTheCredentialsRole(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)

	var (
		canLogin, super, bypassRLS bool
		appIsMember, appHasUsage   bool
	)

	err := superuser.QueryRow(ctx, `
		SELECT r.rolcanlogin,
		       r.rolsuper,
		       r.rolbypassrls,
		       pg_has_role($2, r.oid, 'MEMBER'),
		       pg_has_role($2, r.oid, 'USAGE')
		FROM pg_roles r
		WHERE r.rolname = $1
	`, pgtest.CredentialsRole, pgtest.AppRole).Scan(&canLogin, &super, &bypassRLS, &appIsMember, &appHasUsage)
	if err != nil {
		t.Fatalf("reading the credentials role: %v", err)
	}

	t.Logf("%s: canlogin=%t super=%t bypassrls=%t; %s may SET ROLE to it: %t (inherits: %t)",
		pgtest.CredentialsRole, canLogin, super, bypassRLS, pgtest.AppRole, appIsMember, appHasUsage)

	if canLogin {
		t.Errorf("%s can log in; it is meant to be reachable only from inside its own functions", pgtest.CredentialsRole)
	}

	if super {
		t.Errorf("%s is a superuser", pgtest.CredentialsRole)
	}

	if bypassRLS {
		t.Errorf("%s holds BYPASSRLS", pgtest.CredentialsRole)
	}

	if appIsMember || appHasUsage {
		t.Errorf("%s can act as %s (member=%t usage=%t); it could read every verifier without calling any function",
			pgtest.AppRole, pgtest.CredentialsRole, appIsMember, appHasUsage)
	}

	// The identity role must not be able to assume it either. Two NOLOGIN roles
	// where one can become the other is one role with extra steps.
	var identityIsMember bool

	if err := superuser.QueryRow(ctx,
		`SELECT pg_has_role($1, $2, 'USAGE')`,
		pgtest.IdentityRole, pgtest.CredentialsRole).Scan(&identityIsMember); err != nil {
		t.Fatalf("reading role membership: %v", err)
	}

	t.Logf("%s may act as %s: %t", pgtest.IdentityRole, pgtest.CredentialsRole, identityIsMember)

	if identityIsMember {
		t.Errorf("%s can act as %s; the two pre-tenant roles collapse into one", pgtest.IdentityRole, pgtest.CredentialsRole)
	}
}

// TestNoPreTenantFunctionReturnsTheVerifier is the property ADR 0003 rests on,
// asserted against the catalog rather than against a reading of the SQL.
//
// The verifier is never selectable *by the application* because no function
// hands it back. That is a statement about the shape of every function's result
// type, so it is checked that way: a future function that added `verifier` to a
// RETURNS TABLE would fail here even if every grant in this file stayed as it
// is.
func TestNoPreTenantFunctionReturnsTheVerifier(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)

	rows, err := superuser.Query(ctx, `
		SELECT p.proname, pg_get_function_result(p.oid)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public'
		  AND p.prosecdef
		ORDER BY p.proname
	`)
	if err != nil {
		t.Fatalf("reading function result types: %v", err)
	}

	defer rows.Close()

	seen := 0

	for rows.Next() {
		var name, result string

		if err := rows.Scan(&name, &result); err != nil {
			t.Fatalf("scanning function result: %v", err)
		}

		seen++

		t.Logf("%s returns %s", name, result)

		if bytes.Contains([]byte(result), []byte("verifier")) {
			t.Errorf("%s returns a column named verifier; the stored credential is meant never to leave the database", name)
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading function result types: %v", err)
	}

	if seen != len(preTenantFunctions) {
		t.Fatalf("checked %d definer functions, want %d", seen, len(preTenantFunctions))
	}
}

// TestEveryTableOutsideSystemSchemasHasForcedRLSAndAPolicy extends the
// invariant in isolation_test.go, which asks only about schema public, to
// wherever the schema grows next.
//
// Migration 00005 is the first thing to create a table outside public, and the
// existing catalog test would not have noticed if it had shipped without RLS.
func TestEveryTableOutsideSystemSchemasHasForcedRLSAndAPolicy(t *testing.T) {
	ctx := context.Background()
	superuser := newSuperuserPool(t)

	rows, err := superuser.Query(ctx, `
		SELECT n.nspname, c.relname, c.relrowsecurity, c.relforcerowsecurity,
		       (SELECT count(*) FROM pg_policy p WHERE p.polrelid = c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'r'
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%'
		  AND c.relname <> $1
		ORDER BY n.nspname, c.relname
	`, gooseVersionTable)
	if err != nil {
		t.Fatalf("reading table security across schemas: %v", err)
	}

	defer rows.Close()

	schemas := map[string]int{}

	for rows.Next() {
		var (
			schema, table   string
			enabled, forced bool
			policies        int
		)

		if err := rows.Scan(&schema, &table, &enabled, &forced, &policies); err != nil {
			t.Fatalf("scanning table security: %v", err)
		}

		schemas[schema]++

		t.Logf("%s.%s: rls=%t forced=%t policies=%d", schema, table, enabled, forced, policies)

		if !enabled {
			t.Errorf("%s.%s has no row-level security", schema, table)
		}

		if !forced {
			t.Errorf("%s.%s has RLS but not FORCE; its owner is exempt from every policy on it", schema, table)
		}

		if policies == 0 {
			t.Errorf("%s.%s has RLS with no policy, which denies everything rather than isolating anything", schema, table)
		}
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("reading table security across schemas: %v", err)
	}

	t.Logf("tables per schema: %v", schemas)

	if schemas[pgtest.AuthSchema] == 0 {
		t.Fatalf("no tables found in schema %s; this test would pass if migration 00005 had never run", pgtest.AuthSchema)
	}
}

// TestAPasswordCanBeStoredAndVerifiedWithoutATenant is the functional claim: a
// credential round trip through the serving role, with no tenant set, using
// only the three functions the app may execute.
//
// The wrong-key case is the important half. It has to come back as *no row* —
// the same signal an unknown user gives — because a handler that could tell the
// two apart could leak which one happened.
func TestAPasswordCanBeStoredAndVerifiedWithoutATenant(t *testing.T) {
	ctx := context.Background()
	s := store.New(newPool(t, 2))
	superuser := newSuperuserPool(t)

	user := createThrowawayUser(t, s, superuser)

	salt := randomBytes(t, 16)
	key := randomBytes(t, 32)

	const (
		memoryKiB   = 19456
		iterations  = 2
		parallelism = 1
		keyLength   = 32
	)

	stored, err := store.WithoutTenantValue(ctx, s, store.ReasonRegisterUser,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.CreatePassword(ctx, store.CreatePasswordParams{
				UserID:      user,
				Salt:        salt,
				MemoryKib:   memoryKiB,
				Iterations:  iterations,
				Parallelism: parallelism,
				KeyLength:   keyLength,
				Key:         key,
			})
		})
	if err != nil {
		t.Fatalf("CreatePassword: %v", err)
	}

	if stored != user {
		t.Fatalf("CreatePassword returned %s, want %s", stored, user)
	}

	params, err := store.WithoutTenantValue(ctx, s, store.ReasonPasswordParams,
		func(ctx context.Context, q store.IdentityQuerier) (store.PasswordKDFParams, error) {
			return q.PasswordParams(ctx, user)
		})
	if err != nil {
		t.Fatalf("PasswordParams: %v", err)
	}

	t.Logf("stored params: salt=%d bytes m=%d t=%d p=%d len=%d",
		len(params.Salt), params.MemoryKib, params.Iterations, params.Parallelism, params.KeyLength)

	if !bytes.Equal(params.Salt, salt) {
		t.Errorf("PasswordParams returned a different salt from the one stored")
	}

	if params.MemoryKib != memoryKiB || params.Iterations != iterations ||
		params.Parallelism != parallelism || params.KeyLength != keyLength {
		t.Errorf("PasswordParams = (m=%d t=%d p=%d len=%d), want (m=%d t=%d p=%d len=%d)",
			params.MemoryKib, params.Iterations, params.Parallelism, params.KeyLength,
			memoryKiB, iterations, parallelism, keyLength)
	}

	verified, err := store.WithoutTenantValue(ctx, s, store.ReasonVerifyPassword,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.VerifyPassword(ctx, store.VerifyPasswordParams{UserID: user, Key: key})
		})
	if err != nil {
		t.Fatalf("VerifyPassword with the right key: %v", err)
	}

	if verified != user {
		t.Errorf("VerifyPassword returned %s, want %s", verified, user)
	}

	t.Logf("verify with the correct derived key -> %s", verified)

	// The stored value is sha256 of the key, so replaying it is not a
	// credential. This is the pass-the-hash property ADR 0003 is about, and it
	// is asserted rather than described: sending the digest fails.
	digest := sha256.Sum256(key)

	for _, wrong := range []struct {
		name string
		key  []byte
	}{
		{name: "a different key", key: randomBytes(t, 32)},
		{name: "the stored verifier replayed", key: digest[:]},
		{name: "an empty key", key: []byte{}},
	} {
		_, err := store.WithoutTenantValue(ctx, s, store.ReasonVerifyPassword,
			func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
				return q.VerifyPassword(ctx, store.VerifyPasswordParams{UserID: user, Key: wrong.key})
			})

		t.Logf("verify with %s -> %v", wrong.name, err)

		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("VerifyPassword with %s = %v, want %v", wrong.name, err, store.ErrNotFound)
		}
	}

	// An unknown user is the same empty answer, which is what lets the handler
	// treat "no account" and "wrong password" identically.
	_, err = store.WithoutTenantValue(ctx, s, store.ReasonVerifyPassword,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.VerifyPassword(ctx, store.VerifyPasswordParams{UserID: uuid.New(), Key: key})
		})

	t.Logf("verify for a user that does not exist -> %v", err)

	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("VerifyPassword for an unknown user = %v, want %v", err, store.ErrNotFound)
	}
}

// TestPasswordParamsReportsAnAccountWithNoPasswordAsNoRow pins the signal for
// an account that exists but has never set a password — an invited user, or
// (later) one that authenticates only through an external provider. It has to
// be indistinguishable from an unknown account at this layer, so internal/auth
// can make it indistinguishable to a client.
func TestPasswordParamsReportsAnAccountWithNoPasswordAsNoRow(t *testing.T) {
	ctx := context.Background()
	s := store.New(newPool(t, 2))
	superuser := newSuperuserPool(t)

	user := createThrowawayUser(t, s, superuser)

	_, err := store.WithoutTenantValue(ctx, s, store.ReasonPasswordParams,
		func(ctx context.Context, q store.IdentityQuerier) (store.PasswordKDFParams, error) {
			return q.PasswordParams(ctx, user)
		})

	t.Logf("PasswordParams for an account with no password -> %v", err)

	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("PasswordParams = %v, want %v", err, store.ErrNotFound)
	}
}

// TestAPasswordCannotBeOverwrittenThroughThisPath is "registration only",
// proved rather than asserted. There is no UPDATE grant and no ON CONFLICT
// clause, so a second call raises a unique violation instead of quietly
// replacing a credential — which is what a takeover through a compromised
// registration endpoint would look like.
func TestAPasswordCannotBeOverwrittenThroughThisPath(t *testing.T) {
	ctx := context.Background()
	s := store.New(newPool(t, 2))
	superuser := newSuperuserPool(t)

	user := createThrowawayUser(t, s, superuser)

	create := func(key []byte) error {
		_, err := store.WithoutTenantValue(ctx, s, store.ReasonRegisterUser,
			func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
				return q.CreatePassword(ctx, store.CreatePasswordParams{
					UserID:      user,
					Salt:        randomBytes(t, 16),
					MemoryKib:   19456,
					Iterations:  2,
					Parallelism: 1,
					KeyLength:   32,
					Key:         key,
				})
			})

		return err
	}

	original := randomBytes(t, 32)

	if err := create(original); err != nil {
		t.Fatalf("first CreatePassword: %v", err)
	}

	err := create(randomBytes(t, 32))
	if err == nil {
		t.Fatal("a second CreatePassword succeeded; this path can overwrite a credential")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("the second CreatePassword failed with a non-Postgres error: %v", err)
	}

	t.Logf("second CreatePassword -> SQLSTATE %s: %s", pgErr.Code, pgErr.Message)

	if pgErr.Code != uniqueViolation {
		t.Errorf("second CreatePassword rejected with SQLSTATE %s, want %s", pgErr.Code, uniqueViolation)
	}

	// And the original still verifies, which is the part that matters: a failed
	// overwrite must not have damaged the credential it failed to replace.
	verified, err := store.WithoutTenantValue(ctx, s, store.ReasonVerifyPassword,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.VerifyPassword(ctx, store.VerifyPasswordParams{UserID: user, Key: original})
		})
	if err != nil {
		t.Fatalf("the original key no longer verifies after a rejected overwrite: %v", err)
	}

	if verified != user {
		t.Errorf("VerifyPassword returned %s, want %s", verified, user)
	}
}

// createThrowawayUser creates a global user through the real pre-tenant door
// and removes it when the test ends. The credential row goes with it: the
// foreign key is ON DELETE CASCADE.
func createThrowawayUser(t *testing.T, s *store.Store, superuser *pgxpool.Pool) uuid.UUID {
	t.Helper()

	ctx := context.Background()
	email := "cred-" + uuid.NewString() + "@example.com"

	created, err := store.WithoutTenantValue(ctx, s, store.ReasonRegisterUser,
		func(ctx context.Context, q store.IdentityQuerier) (store.CreatedUser, error) {
			return q.CreateUser(ctx, store.CreateUserParams{Email: email, DisplayName: "Credential Fixture"})
		})
	if err != nil {
		t.Fatalf("creating a throwaway user: %v", err)
	}

	t.Cleanup(func() {
		if _, derr := superuser.Exec(ctx, `DELETE FROM users WHERE id = $1`, created.ID); derr != nil {
			t.Errorf("removing the throwaway user: %v", derr)
		}
	})

	return created.ID
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()

	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("reading %d random bytes: %v", n, err)
	}

	return b
}
