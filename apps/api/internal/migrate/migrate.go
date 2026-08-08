// Package migrate applies the embedded goose migrations.
//
// Migrations run as a separate command rather than on API startup. Three
// reasons, in order of importance:
//
//  1. They need a different database identity. The migrating role owns the
//     tables; the serving role (collabboard_app) deliberately owns nothing and
//     cannot run DDL — that separation is what makes the RLS policies real
//     (docs/adr/0001-tenant-isolation.md). A startup hook would have to migrate
//     as the serving role, which either fails or forces the serving role to be
//     over-privileged.
//  2. A rolling deploy starts N tasks at once. Migrating at startup means N
//     concurrent attempts, and a task that crash-loops on a bad migration takes
//     the service down with it. As a pre-deploy step, a failed migration fails
//     the deploy with the old version still serving.
//  3. Rollback stays possible without shipping a new image.
//
// The tradeoff is one more moving part in the deploy pipeline: `api migrate up`
// has to be wired in as a task that runs before the service updates, and
// forgetting it means the new code meets the old schema.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	// Registers the "pgx" database/sql driver. goose needs a *sql.DB, while the
	// API itself uses the pgx-native pool; this is the only place the stdlib
	// interface is used.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/AndyV99/collabboard/apps/api/migrations"
)

// Command is a migration operation. The names mirror the goose CLI so that
// what the binary does and what `goose` does are the same thing under the same
// word — `down` is one step, `reset` is all the way back.
type Command string

// The supported migration commands.
const (
	CommandUp     Command = "up"
	CommandDown   Command = "down"
	CommandReset  Command = "reset"
	CommandStatus Command = "status"
)

// ErrUnknownCommand is returned for an unrecognised subcommand argument.
var ErrUnknownCommand = errors.New("unknown migrate command")

// ErrExemptMigrationRole is returned when the connected role is one that
// row-level security is not enforced against. See [preflight].
var ErrExemptMigrationRole = errors.New("migration role is exempt from row-level security")

// Commands lists the supported commands, for usage messages.
func Commands() []Command {
	return []Command{CommandUp, CommandDown, CommandReset, CommandStatus}
}

// Valid reports whether cmd is one of the supported commands.
func (c Command) Valid() bool {
	for _, known := range Commands() {
		if c == known {
			return true
		}
	}

	return false
}

// Run opens a connection with the supplied DSN, which must belong to the role
// that owns the schema, and executes cmd. The connection is closed before Run
// returns.
func Run(ctx context.Context, logger *slog.Logger, dsn string, cmd Command) error {
	// Checked before the connection is opened, so a typo fails immediately and
	// visibly rather than after a connection timeout against a real database.
	if !cmd.Valid() {
		return fmt.Errorf("%w: %q (want one of %v)", ErrUnknownCommand, cmd, Commands())
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("opening database for migrations: %w", err)
	}

	defer func() {
		if cerr := db.Close(); cerr != nil {
			logger.Error("closing migration database handle", slog.Any("error", cerr))
		}
	}()

	// A Postgres advisory lock held for the session serialises concurrent
	// runs, so two deploy tasks racing to migrate cannot interleave.
	sessionLocker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("creating migration session locker: %w", err)
	}

	if err := preflight(ctx, logger, db); err != nil {
		return err
	}

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		db,
		migrations.FS,
		goose.WithSessionLocker(sessionLocker),
		goose.WithSlog(logger),
	)
	if err != nil {
		return fmt.Errorf("creating migration provider: %w", err)
	}

	return dispatch(ctx, logger, provider, cmd)
}

// preflightSQL asks whether row-level security is enforced against the role
// this connection authenticated as.
//
// to_regclass returns NULL rather than raising for a relation that does not
// exist, so this is also the "is there a schema yet" probe: on an empty database
// there is no users table to ask about, and the attribute columns are all the
// answer available.
const preflightSQL = `
SELECT r.rolname,
       r.rolsuper,
       r.rolbypassrls,
       to_regclass('public.users') IS NOT NULL AS schema_present,
       CASE
           WHEN to_regclass('public.users') IS NOT NULL
           THEN pg_catalog.row_security_active('public.users')
           ELSE true
       END AS rls_enforced
FROM pg_catalog.pg_roles r
WHERE r.rolname = CURRENT_USER`

// preflight refuses to migrate as a role that row-level security is not
// enforced against, before any migration runs.
//
// # Why this is in Go as well as in SQL
//
// Migration 00006 makes the same check, and it has to: it is what travels with
// the schema, and it is what catches a chain applied by anything other than
// this binary. But goose applies each migration in its own transaction, so a
// check that lives in the last file only fires after the first five have
// already been applied — by the wrong role, leaving every table owned by it.
// Recovering means reassigning ownership, which is a documented step
// (apps/api/scripts/provision/bootstrap-owner.sql takes -v previous_owner) but
// not one anybody should be walked into by a stale environment variable.
//
// Checking here costs one round trip and means the answer to "I ran it as the
// wrong role" is "nothing happened".
//
// # What it checks
//
// The same thing 00006 checks, for the same reasons — see that file's header.
// The short version: attributes alone would miss the RDS master user, which is
// neither a superuser nor BYPASSRLS and is exactly the identity this is meant
// to stop. row_security_active answers the question directly, and the attribute
// columns are read anyway so the error can say which of them is the problem.
//
// On an empty database there is no table to ask about, so only the attributes
// are available. That is enough for the case that matters there: a fresh
// database migrated by the compose stack's bootstrap superuser.
func preflight(ctx context.Context, logger *slog.Logger, db *sql.DB) error {
	var (
		role          string
		isSuper       bool
		bypassesRLS   bool
		schemaPresent bool
		rlsEnforced   bool
	)

	err := db.QueryRowContext(ctx, preflightSQL).
		Scan(&role, &isSuper, &bypassesRLS, &schemaPresent, &rlsEnforced)
	if err != nil {
		return fmt.Errorf("checking the migration role: %w", err)
	}

	if !isSuper && !bypassesRLS && rlsEnforced {
		logger.Info("migration role checked",
			slog.String("role", role),
			slog.Bool("schema_present", schemaPresent),
		)

		return nil
	}

	return fmt.Errorf(
		"%w: connected as %q (rolsuper=%t, rolbypassrls=%t, row-level security enforced=%t). "+
			"Migrations must run as a dedicated non-superuser owner, or every policy in the schema is decorative for the role that installed it. "+
			"Provision one with apps/api/scripts/provision/bootstrap-owner.sql and point POSTGRES_MIGRATION_USER at it; see docs/adr/0005-database-role-provisioning.md",
		ErrExemptMigrationRole, role, isSuper, bypassesRLS, rlsEnforced)
}

func dispatch(ctx context.Context, logger *slog.Logger, provider *goose.Provider, cmd Command) error {
	switch cmd {
	case CommandUp:
		results, err := provider.Up(ctx)

		return report(logger, "applied", results, err)
	case CommandDown:
		result, err := provider.Down(ctx)
		if err != nil {
			return fmt.Errorf("rolling back one migration: %w", err)
		}

		return report(logger, "rolled back", []*goose.MigrationResult{result}, nil)
	case CommandReset:
		results, err := provider.DownTo(ctx, 0)

		return report(logger, "rolled back", results, err)
	case CommandStatus:
		return status(ctx, logger, provider)
	default:
		return fmt.Errorf("%w: %q (want one of %v)", ErrUnknownCommand, cmd, Commands())
	}
}

func report(logger *slog.Logger, verb string, results []*goose.MigrationResult, err error) error {
	// Logged before the error check on purpose: a partial failure still applied
	// everything before it, and knowing where it stopped is the whole point.
	for _, result := range results {
		if result == nil {
			continue
		}

		logger.Info("migration "+verb,
			slog.Int64("version", result.Source.Version),
			slog.String("path", result.Source.Path),
			slog.Duration("duration", result.Duration),
		)
	}

	if err != nil {
		return fmt.Errorf("migrating: %w", err)
	}

	if len(results) == 0 {
		logger.Info("no migrations to apply")
	}

	return nil
}

func status(ctx context.Context, logger *slog.Logger, provider *goose.Provider) error {
	statuses, err := provider.Status(ctx)
	if err != nil {
		return fmt.Errorf("reading migration status: %w", err)
	}

	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("reading database version: %w", err)
	}

	logger.Info("migration status", slog.Int64("db_version", version))

	for _, s := range statuses {
		attrs := []any{
			slog.Int64("version", s.Source.Version),
			slog.String("path", s.Source.Path),
			slog.String("state", string(s.State)),
		}
		if !s.AppliedAt.IsZero() {
			attrs = append(attrs, slog.Time("applied_at", s.AppliedAt))
		}

		logger.Info("migration", attrs...)
	}

	return nil
}
