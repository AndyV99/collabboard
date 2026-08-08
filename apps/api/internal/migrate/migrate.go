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
