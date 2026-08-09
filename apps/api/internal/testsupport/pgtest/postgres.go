//go:build integration

package pgtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/AndyV99/collabboard/apps/api/internal/migrate"
	"github.com/AndyV99/collabboard/apps/api/internal/provision"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

const (
	// Image is pinned to the same major version as docker-compose.yml. The
	// schema uses ON DELETE SET NULL (column_list), which is Postgres 15+, so
	// the version is load-bearing rather than incidental.
	Image = "postgres:16-alpine"

	// SuperuserRole is the role the container starts with: the cluster's
	// bootstrap superuser.
	//
	// It provisions [SchemaOwnerRole] — the one step a migration cannot do for
	// itself, because creating a role that must not be a superuser requires
	// privileges that role must not have — and it seeds fixtures. Nothing under
	// test ever runs through it, and nothing about the schema is asserted
	// through it either.
	//
	// Fixtures need it and cannot avoid it. Seeding is precisely what the
	// policies forbid: `users` cannot be inserted into by any role subject to
	// RLS, because its WITH CHECK requires a membership that cannot exist until
	// the user row does. That is issue #13 showing up where migration 00002's
	// header says it will, not a shortcut.
	SuperuserRole = "collabboard"

	// SchemaOwnerRole owns the schema and runs the migrations.
	//
	// Not a superuser and no BYPASSRLS, so FORCE ROW LEVEL SECURITY actually
	// applies to it. Before issue #14 the migrations ran as SuperuserRole, and
	// the whole chain had come to depend on that without anyone choosing it:
	// three of the five migrations issued ALTER ROLE statements PostgreSQL only
	// permits a superuser to issue. A harness that migrates as a superuser
	// cannot notice.
	SchemaOwnerRole = "collabboard_owner"

	// AppRole is the serving role that migration 00001 creates: no superuser,
	// no BYPASSRLS, owns nothing, table grants only.
	AppRole = "collabboard_app"

	// IdentityRole owns the pre-tenant identity functions that migration 00004
	// creates. Nothing connects as it — it cannot log in — so there is no DSN
	// here to go with it. It appears in the harness only so that tests can ask
	// the catalog what it is and is not allowed to touch.
	IdentityRole = "collabboard_identity"

	// CredentialsRole owns the three password functions that migration 00005
	// creates. Like IdentityRole it cannot log in and has no DSN. It is a
	// separate role from IdentityRole precisely so that the identity path
	// cannot read a password verifier and the credential path cannot read an
	// email — see docs/adr/0003-password-verifier-storage.md.
	//nolint:gosec // G101 pattern-matches the word "credentials"; this is a
	// Postgres role name, and the role has no password by construction.
	CredentialsRole = "collabboard_credentials"

	// AuthSchema holds credential storage. The serving role deliberately has no
	// USAGE on it, which is a stronger boundary than a row-level policy: it
	// cannot name the table at all.
	AuthSchema = "auth"

	// Database is the database the migrations are applied to.
	Database = "collabboard"
)

// startTimeout bounds the whole bring-up: pull, start, wait-for-ready and
// migrate. Generous, because the first run on a cold machine has to pull the
// image, but finite so a wedged daemon fails the suite instead of hanging it.
const startTimeout = 5 * time.Minute

// pingTimeout bounds the "is this pool usable" probe.
const pingTimeout = 10 * time.Second

// bootstrapScriptPath locates the provisioning SQL a deployed environment runs,
// relative to this file.
//
// The harness executes that file rather than restating its SQL, for the same
// reason [Start] calls migrate.Run rather than reimplementing goose: a harness
// that proves the migrations work under a role model no operator will ever
// create proves nothing. The script is an artifact under test.
//
// It is resolved through runtime.Caller rather than go:embed because embed
// patterns cannot escape the package directory, and rather than a path relative
// to the working directory because `go test` sets that to whichever package is
// running — internal/store here, internal/api there. The compiled-in source
// path is the one thing that is the same for all of them.
const bootstrapScriptPath = "../../../scripts/provision/bootstrap-owner.sql"

// initScriptPath is the compose stack's init hook, also relative to this file.
// The harness runs the same one, so "docker compose up produces a database the
// migrations can be applied to" is a claim CI checks.
const initScriptPath = "../../../scripts/dev/initdb/10-bootstrap-owner.sh"

// containerProvisionDir is where the init hook looks for the provisioning SQL.
// It matches the mount point in docker-compose.yml; the two are the same
// contract seen from two sides.
const containerProvisionDir = "/opt/collabboard/provision"

// ownerPasswordEnv is the environment variable the init hook reads the schema
// owner's password from. Named here so that renaming it in the hook breaks the
// harness loudly rather than leaving it to fall back to the local default.
//
//nolint:gosec // G101 pattern-matches "PASSWORD"; this is the variable's name, not its value.
const ownerPasswordEnv = "COLLABBOARD_OWNER_PASSWORD"

// DB is a running Postgres container with the migrations applied.
//
// The three DSNs are the point of the type: which one a test picks decides
// whether it is proving anything.
type DB struct {
	container *postgres.PostgresContainer

	// SuperuserDSN connects as the cluster's bootstrap superuser. It exists to
	// create the schema owner and to seed fixtures the policies would otherwise
	// forbid — creating a user, for instance, which no role subject to RLS can
	// do because the membership that would make the user visible cannot exist
	// yet. Nothing under test runs through it.
	SuperuserDSN string

	// SchemaOwnerDSN connects as collabboard_owner: the non-superuser that owns
	// every table and runs the migrations. Row-level security applies to it,
	// which is exactly what makes it worth migrating as.
	SchemaOwnerDSN string

	// AppDSN connects as collabboard_app. Everything under test runs through
	// this one.
	AppDSN string

	// fixtureOnce guards the long-lived superuser pool that Seed and
	// SuperuserExec share. It belongs to the harness rather than to a test,
	// because fixtures are created and torn down across several tests and a
	// pool closed by the first test's cleanup would break the second's.
	fixtureOnce sync.Once
	fixturePool *pgxpool.Pool
	fixtureErr  error
}

// Start brings up Postgres on a random host port, provisions the schema owner,
// applies the embedded migrations as that owner, and gives the app role a
// password.
//
// Every step after the container start runs the code a deploy runs: the compose
// stack's init hook, the provisioning SQL an operator executes, migrate.Run and
// provision.Roles. Nothing about the role model is reimplemented here, because a
// harness that builds the schema its own way can only ever prove things about
// itself.
//
// The caller owns the returned [DB] and must call [DB.Close]. On any failure
// during bring-up the container is terminated before the error is returned, so
// a half-started harness does not leak a container.
func Start(ctx context.Context) (*DB, error) {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	// Generated per run rather than hardcoded. Nothing outside this process
	// needs them, the container is discarded when the run ends, and a constant
	// here would be a committed credential that looks like it means something.
	superuserPassword := uuid.NewString()
	ownerPassword := uuid.NewString()
	appPassword := uuid.NewString()

	initScript, provisionScript, err := provisionScripts()
	if err != nil {
		return nil, err
	}

	container, err := postgres.Run(ctx, Image,
		postgres.WithDatabase(Database),
		postgres.WithUsername(SuperuserRole),
		postgres.WithPassword(superuserPassword),
		// The compose stack's own init hook, run the way the compose stack runs
		// it: the image executes everything in /docker-entrypoint-initdb.d once,
		// on an empty data directory, as the bootstrap superuser. Using it here
		// means "the local dev path works" is asserted by CI rather than by
		// whoever last followed the README.
		postgres.WithInitScripts(initScript),
		// The hook shells out to this file, which is the same one a deployed
		// environment runs. It is placed where the hook looks for it — the path
		// docker-compose.yml mounts it at — rather than in initdb.d, where the
		// entrypoint would also execute it directly and without arguments.
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath:      provisionScript,
			ContainerFilePath: containerProvisionDir + "/" + filepath.Base(provisionScript),
			FileMode:          0o644,
		}),
		testcontainers.WithEnv(map[string]string{
			ownerPasswordEnv: ownerPassword,
		}),
		// Waits for the "ready to accept connections" log twice — Postgres
		// emits it once for the init-scripts phase and once for the real
		// startup, and connecting during the first one hits a database that is
		// about to be shut down and restarted.
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		// Run can return a non-nil container alongside an error (started, then
		// failed its wait strategy), so termination is attempted regardless.
		return nil, errors.Join(
			fmt.Errorf("starting postgres container: %w", err),
			testcontainers.TerminateContainer(container),
		)
	}

	db := &DB{container: container}

	superuserDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, db.abort(fmt.Errorf("reading connection string: %w", err))
	}

	db.SuperuserDSN = superuserDSN

	// Same host and port, different identity. The owner role was created by the
	// init hook above, before the database finished starting.
	schemaOwnerDSN, err := withCredentials(superuserDSN, SchemaOwnerRole, ownerPassword)
	if err != nil {
		return nil, db.abort(err)
	}

	db.SchemaOwnerDSN = schemaOwnerDSN

	// The real migration code, not a reimplementation: whatever this harness
	// proves about the schema is only worth something if the schema got there
	// the way a deploy would. As of issue #14 that includes the identity it got
	// there under — migration 00006 refuses to apply as anything row-level
	// security is not enforced against, so a regression that put the superuser
	// DSN back here fails the whole suite at bring-up.
	if err := migrate.Run(ctx, harnessLogger(), schemaOwnerDSN, migrate.CommandUp); err != nil {
		return nil, db.abort(fmt.Errorf("applying migrations as %s: %w", SchemaOwnerRole, err))
	}

	// Migration 00001 creates collabboard_app without a password on purpose, so
	// a freshly migrated database has an app role that cannot log in. This is
	// `api provision`, called as a library rather than restated: a container
	// that lives for one test run gets its credential from configuration the
	// same way a deployed environment does.
	if err := provision.Roles(ctx, harnessLogger(), schemaOwnerDSN, provision.Credential{
		Role:     AppRole,
		Password: appPassword,
	}); err != nil {
		return nil, db.abort(fmt.Errorf("provisioning the %s password: %w", AppRole, err))
	}

	appDSN, err := withCredentials(superuserDSN, AppRole, appPassword)
	if err != nil {
		return nil, db.abort(err)
	}

	db.AppDSN = appDSN

	return db, nil
}

// provisionScripts resolves the two files the container needs from this
// package's own source location.
//
// runtime.Caller rather than a working-directory-relative path: `go test` sets
// the working directory to the package under test, which is internal/store for
// one suite and internal/api for another, so nothing relative to it is stable.
// The compiled-in path of this file is.
func provisionScripts() (initScript, provisionScript string, err error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", "", errors.New("pgtest: cannot locate this package's source; the provisioning scripts are read from disk")
	}

	dir := filepath.Dir(thisFile)

	initScript = filepath.Join(dir, initScriptPath)
	provisionScript = filepath.Join(dir, bootstrapScriptPath)

	for _, path := range []string{initScript, provisionScript} {
		if _, statErr := os.Stat(path); statErr != nil {
			return "", "", fmt.Errorf("pgtest: reading provisioning script %s: %w", path, statErr)
		}
	}

	return initScript, provisionScript, nil
}

// AppStore opens a pool as the serving role and wraps it in the real
// [store.Store].
//
// It exists so that a test outside internal/store can exercise code taking a
// *store.Store without importing pgx — which .golangci.yml's depguard rule
// forbids in internal/api and internal/realtime, for the reason ADR 0001 gives.
// Without it, the WebSocket hub's integration test would have to reach for the
// driver to build the very dependency whose whole job is to keep it away from
// the driver.
func (db *DB) AppStore(tb testing.TB, maxConns int32) *store.Store {
	tb.Helper()

	return store.New(db.AppPool(tb, maxConns))
}

// Seed creates the two-tenant fixture using the harness's own superuser pool.
func (db *DB) Seed(tb testing.TB) Fixture {
	tb.Helper()

	return SeedTenants(tb, db.fixtures(tb))
}

// SuperuserExec runs one statement as the bootstrap superuser.
//
// For the fixture changes every other identity is deliberately forbidden from
// making — revoking a membership, for instance, which is how the realtime suite
// tests what happens to a live WebSocket when one goes away. Since issue #14
// that includes the schema owner: it is subject to FORCE ROW LEVEL SECURITY, so
// the same DELETE issued through [DB.SchemaOwnerPool] would silently affect
// zero rows.
func (db *DB) SuperuserExec(tb testing.TB, sql string, args ...any) {
	tb.Helper()

	if _, err := db.fixtures(tb).Exec(context.Background(), sql, args...); err != nil {
		tb.Fatalf("pgtest: executing as %s (%s): %v", SuperuserRole, sql, err)
	}
}

// OwnerExec is the pre-#14 name for [DB.SuperuserExec], and a misnomer since.
//
// It was accurate when the container's bootstrap superuser was also the schema
// owner. It is not any more — the schema owner is [SchemaOwnerRole] — and this
// method has never meant that role. Kept only because internal/realtime spells
// it this way and #45 is in flight over that package. Do not use it in new
// code; it goes away once that lands.
//
// Not marked with a `Deprecated:` line on purpose: staticcheck would then fail
// the lint run on a package this change is not allowed to touch, which would
// hold this work hostage to another branch's merge order.
func (db *DB) OwnerExec(tb testing.TB, sql string, args ...any) {
	tb.Helper()

	db.SuperuserExec(tb, sql, args...)
}

// fixtures returns the harness-owned superuser pool, opening it on first use.
// It is closed by [DB.Close] rather than by a test's cleanup, because it
// outlives any one test.
func (db *DB) fixtures(tb testing.TB) *pgxpool.Pool {
	tb.Helper()

	if db == nil || db.SuperuserDSN == "" {
		tb.Fatal("pgtest: harness was never started; TestMain did not run or failed")
	}

	db.fixtureOnce.Do(func() {
		cfg, err := pgxpool.ParseConfig(db.SuperuserDSN)
		if err != nil {
			db.fixtureErr = fmt.Errorf("parsing the superuser dsn: %w", err)

			return
		}

		cfg.MaxConns = 4

		db.fixturePool, db.fixtureErr = pgxpool.NewWithConfig(context.Background(), cfg)
	})

	if db.fixtureErr != nil {
		tb.Fatalf("pgtest: opening the fixture pool: %v", db.fixtureErr)
	}

	return db.fixturePool
}

// Close terminates the container. It is safe on a nil or partially built DB, so
// the caller can defer it immediately.
//
// Termination is best-effort by design: if the process dies before this runs —
// a panic in a test goroutine takes the process down without unwinding main —
// Testcontainers' reaper container (Ryuk) removes anything labelled for this
// session once the client's connection to it drops. That is the backstop that
// makes "torn down even when tests fail" true rather than aspirational.
func (db *DB) Close() error {
	if db == nil || db.container == nil {
		return nil
	}

	if db.fixturePool != nil {
		db.fixturePool.Close()
	}

	if err := testcontainers.TerminateContainer(db.container); err != nil {
		return fmt.Errorf("terminating postgres container: %w", err)
	}

	return nil
}

// AppPool opens a pool as the serving role and closes it when the test ends.
//
// maxConns is a parameter because some tests only mean something at exactly
// one connection: with a pool of one, "the pool gave the connection back" and
// "the next query ran on the same backend" are observable facts rather than
// probabilities.
func (db *DB) AppPool(tb testing.TB, maxConns int32) *pgxpool.Pool {
	tb.Helper()

	return db.openPool(tb, db.AppDSN, maxConns)
}

// SuperuserPool opens a pool as the bootstrap superuser, for seeding fixtures
// and for reading the catalog. Nothing under test should run through it: RLS is
// not enforced against a superuser, so an assertion made here passes whether
// the policies work or not.
func (db *DB) SuperuserPool(tb testing.TB, maxConns int32) *pgxpool.Pool {
	tb.Helper()

	return db.openPool(tb, db.SuperuserDSN, maxConns)
}

// SchemaOwnerPool opens a pool as collabboard_owner, the non-superuser that
// owns every table and applied the migrations.
//
// Unlike [DB.SuperuserPool] this identity is worth asserting against: FORCE ROW
// LEVEL SECURITY applies to it, so "the owner with no tenant context sees
// nothing" is a real claim about the schema rather than a property of the
// connection string. It is the identity a bug that pointed the API at the
// migration credentials would use, which is why the suite checks what it can
// and cannot see.
func (db *DB) SchemaOwnerPool(tb testing.TB, maxConns int32) *pgxpool.Pool {
	tb.Helper()

	return db.openPool(tb, db.SchemaOwnerDSN, maxConns)
}

// OwnerPool is the pre-#14 name for [DB.SuperuserPool], and a misnomer since.
//
// It was accurate when the container's bootstrap superuser was also the schema
// owner. Since issue #14 those are different roles, and this method has never
// returned the schema owner's — [DB.SchemaOwnerPool] does. Kept only because
// internal/api spells it this way and #45 is in flight over that package. Do
// not use it in new code; it goes away once that lands. See [DB.OwnerExec] for
// why it carries no `Deprecated:` marker.
func (db *DB) OwnerPool(tb testing.TB, maxConns int32) *pgxpool.Pool {
	tb.Helper()

	return db.SuperuserPool(tb, maxConns)
}

func (db *DB) openPool(tb testing.TB, dsn string, maxConns int32) *pgxpool.Pool {
	tb.Helper()

	if db == nil || dsn == "" {
		tb.Fatal("pgtest: harness was never started; TestMain did not run or failed")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		tb.Fatalf("pgtest: parsing dsn: %v", err)
	}

	cfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		tb.Fatalf("pgtest: creating pool: %v", err)
	}

	tb.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	// Fatal, not Skip. The container is already up by the time any test asks
	// for a pool, so an unreachable database here is a broken harness, and a
	// harness that skips when it breaks is worse than no harness.
	if err := pool.Ping(ctx); err != nil {
		tb.Fatalf("pgtest: pinging %s: %v", redactDSN(dsn), err)
	}

	return pool
}

// abort terminates the container and joins any termination failure onto the
// error that caused the abort, so a bring-up failure never leaks a container.
func (db *DB) abort(cause error) error {
	return errors.Join(cause, db.Close())
}

// withCredentials swaps the user and password of a DSN, keeping the host, port
// and options the container chose.
func withCredentials(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parsing dsn: %w", err)
	}

	u.User = url.UserPassword(user, password)

	return u.String(), nil
}

// redactDSN strips credentials from a DSN so a failure message can name the
// server without printing the password into CI logs.
func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "postgres://<unparseable>"
	}

	if u.User != nil {
		u.User = url.User(u.User.Username())
	}

	return u.String()
}

// harnessLogger sends goose's output to stderr at info level. Three lines per
// run, and they are the evidence that the schema under test was built by the
// real migrations.
func harnessLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
