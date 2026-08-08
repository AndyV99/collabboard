//go:build integration

package pgtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/AndyV99/collabboard/apps/api/internal/migrate"
)

const (
	// Image is pinned to the same major version as docker-compose.yml. The
	// schema uses ON DELETE SET NULL (column_list), which is Postgres 15+, so
	// the version is load-bearing rather than incidental.
	Image = "postgres:16-alpine"

	// OwnerRole owns the schema and runs the migrations. In the container it is
	// also the bootstrap superuser, which is fine precisely because no test
	// asserts anything through it.
	OwnerRole = "collabboard"

	// AppRole is the serving role that migration 00001 creates: no superuser,
	// no BYPASSRLS, owns nothing, table grants only.
	AppRole = "collabboard_app"

	// IdentityRole owns the pre-tenant identity functions that migration 00004
	// creates. Nothing connects as it — it cannot log in — so there is no DSN
	// here to go with it. It appears in the harness only so that tests can ask
	// the catalog what it is and is not allowed to touch.
	IdentityRole = "collabboard_identity"

	// Database is the database the migrations are applied to.
	Database = "collabboard"
)

// startTimeout bounds the whole bring-up: pull, start, wait-for-ready and
// migrate. Generous, because the first run on a cold machine has to pull the
// image, but finite so a wedged daemon fails the suite instead of hanging it.
const startTimeout = 5 * time.Minute

// pingTimeout bounds the "is this pool usable" probe.
const pingTimeout = 10 * time.Second

// alterAppRoleLoginSQL gives the app role a login password.
//
// Migration 00001 creates collabboard_app without one on purpose — a credential
// in a versioned migration can never be rotated — so a freshly migrated
// database has an app role that cannot log in. Deployed environments set it
// from the secret store; local dev uses scripts/dev/set-app-role-password.sql;
// this is the same step for a container that lives for one test run.
//
// ALTER ROLE takes a literal password and cannot be parameterised, and
// interpolating into SQL is how injection happens. The value therefore travels
// as a bind parameter into a GUC, and format(%L) quotes it back out inside a DO
// block, which is the parameterised equivalent.
const alterAppRoleLoginSQL = `
DO $$
BEGIN
    EXECUTE format('ALTER ROLE %I WITH PASSWORD %L',
                   current_setting('pgtest.app_role'),
                   current_setting('pgtest.app_password'));
END
$$`

// DB is a running Postgres container with the migrations applied.
//
// The two DSNs are the point of the type: which one a test picks decides
// whether it is proving anything.
type DB struct {
	container *postgres.PostgresContainer

	// OwnerDSN connects as the role that owns the schema. For migrations and
	// for seeding fixtures the policies would otherwise forbid — creating a
	// user, for instance, which no tenant-scoped transaction can do because the
	// membership that would make the user visible cannot exist yet.
	OwnerDSN string

	// AppDSN connects as collabboard_app. Everything under test runs through
	// this one.
	AppDSN string
}

// Start brings up Postgres on a random host port, applies the embedded
// migrations as the owner, and gives the app role a password.
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
	ownerPassword := uuid.NewString()
	appPassword := uuid.NewString()

	container, err := postgres.Run(ctx, Image,
		postgres.WithDatabase(Database),
		postgres.WithUsername(OwnerRole),
		postgres.WithPassword(ownerPassword),
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

	ownerDSN, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, db.abort(fmt.Errorf("reading connection string: %w", err))
	}

	db.OwnerDSN = ownerDSN

	// The real migration code, not a reimplementation: whatever this harness
	// proves about the schema is only worth something if the schema got there
	// the way a deploy would.
	if err := migrate.Run(ctx, harnessLogger(), ownerDSN, migrate.CommandUp); err != nil {
		return nil, db.abort(fmt.Errorf("applying migrations: %w", err))
	}

	if err := setAppRolePassword(ctx, ownerDSN, appPassword); err != nil {
		return nil, db.abort(err)
	}

	appDSN, err := withCredentials(ownerDSN, AppRole, appPassword)
	if err != nil {
		return nil, db.abort(err)
	}

	db.AppDSN = appDSN

	return db, nil
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

// OwnerPool opens a pool as the schema owner, for seeding and for reading the
// catalog. Nothing under test should run through it.
func (db *DB) OwnerPool(tb testing.TB, maxConns int32) *pgxpool.Pool {
	tb.Helper()

	return db.openPool(tb, db.OwnerDSN, maxConns)
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

func setAppRolePassword(ctx context.Context, ownerDSN, password string) error {
	conn, err := pgx.Connect(ctx, ownerDSN)
	if err != nil {
		return fmt.Errorf("connecting as %s to set the app role password: %w", OwnerRole, err)
	}

	defer func() { _ = conn.Close(ctx) }()

	for _, setting := range []struct{ name, value string }{
		{"pgtest.app_role", AppRole},
		{"pgtest.app_password", password},
	} {
		if _, err := conn.Exec(ctx, `SELECT set_config($1, $2, false)`, setting.name, setting.value); err != nil {
			return fmt.Errorf("setting %s: %w", setting.name, err)
		}
	}

	if _, err := conn.Exec(ctx, alterAppRoleLoginSQL); err != nil {
		return fmt.Errorf("setting the %s password: %w", AppRole, err)
	}

	return nil
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
