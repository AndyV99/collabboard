// Package provision applies the database credentials that migrations
// deliberately do not contain.
//
// # What this exists instead of
//
// Migration 00001 creates collabboard_app without a password on purpose: a
// credential written into a versioned migration is a credential that can never
// be rotated, and that every environment which ever ran that migration shares.
// Until issue #14 the gap was filled by a checked-in SQL file that set the
// password to the literal "dev", with a comment explaining that deployed
// environments would do something else. Nothing did the something else.
//
// This package is the something else. The password comes from the same
// configuration value the API connects with — POSTGRES_PASSWORD — so there is
// one secret rather than two things that have to agree, and rotating it is
// "update the secret, run `api provision`, roll the tasks" rather than a
// migration.
//
// # Where a secret manager plugs in
//
// It already does, and that is the honest shape rather than a missing piece.
// The value arrives in the process environment; in ECS that is a `secrets:`
// entry pointing at a Secrets Manager ARN, which the agent resolves before the
// container starts. There is no SecretSource interface here because there is
// nothing for a second implementation to do: the injection happens outside the
// process, which is what keeps the secret out of the image, out of the task
// definition and out of this repository. See
// docs/adr/0006-database-role-provisioning.md.
//
// # Ordering
//
// `api provision` runs after `api migrate up`, because it operates on roles the
// migrations create. Both connect as the schema owner. Rotation has a real
// ordering hazard that no amount of code here removes: changing the password
// invalidates it for every task still holding the old one, so a rotation is a
// deploy, not a background job.
package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ErrInvalidCredential is returned for a credential this package refuses to
// apply before it opens a connection.
var ErrInvalidCredential = errors.New("invalid role credential")

// Credential is one role and the password it should end up with.
type Credential struct {
	// Role is the database role name. It is written into DDL, so it is checked
	// rather than trusted — see [Credential.Validate].
	Role string

	// Password is the secret the role authenticates with. Never logged.
	Password string
}

// maxIdentifierLength is PostgreSQL's NAMEDATALEN - 1. A longer role name is
// silently truncated by the server, which would mean altering a different role
// than the one configured.
const maxIdentifierLength = 63

// Validate reports whether the credential can be applied at all.
//
// The role name is constrained to the characters an unquoted PostgreSQL
// identifier may contain. That is stricter than PostgreSQL requires — a quoted
// identifier may contain anything — and deliberately so: the role names this
// service provisions are all of the form collabboard_*, and a name that needs
// quoting to be safe is a name that arrived from somewhere it should not have.
// The DDL is built with format(%I) on the server as well, so this is the outer
// of two checks rather than the only one.
func (c Credential) Validate() error {
	switch {
	case c.Role == "":
		return fmt.Errorf("%w: role is empty", ErrInvalidCredential)
	case len(c.Role) > maxIdentifierLength:
		return fmt.Errorf("%w: role %q is %d bytes, PostgreSQL truncates at %d",
			ErrInvalidCredential, c.Role, len(c.Role), maxIdentifierLength)
	case !isPlainIdentifier(c.Role):
		return fmt.Errorf("%w: role %q is not a plain lower-case identifier", ErrInvalidCredential, c.Role)
	case c.Password == "":
		// Not a stylistic objection. ALTER ROLE ... PASSWORD '' sets the role's
		// password to null, which turns a role that could not be logged into
		// without the secret into one that md5/scram authentication rejects but
		// `trust` or `peer` would let straight in.
		return fmt.Errorf("%w: password for role %q is empty; that would clear the password rather than set one",
			ErrInvalidCredential, c.Role)
	}

	return nil
}

func isPlainIdentifier(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r == '_':
		case i > 0 && (r >= '0' && r <= '9'):
		default:
			return false
		}
	}

	return true
}

// setPasswordSQL applies one credential.
//
// The password never appears in this string. ALTER ROLE takes a literal and
// cannot be parameterised, and interpolating a secret into SQL in Go is how
// injection happens, so the value travels as a bind parameter into a
// transaction-local GUC and format(%L) quotes it back out on the server.
// set_config's third argument is true, so the setting is discarded at commit
// rather than lingering on a pooled connection.
//
// The two refusals in the middle are the part worth reading. Handing a login
// credential to a role that is a superuser or holds BYPASSRLS would create
// exactly the failure ADR 0001 calls the superuser trap — a connection string
// the API can use that no policy applies to — and it would do it from a
// configuration change with no migration and no review. Refusing here means the
// mistake surfaces as a failed deploy rather than as a silent loss of tenant
// isolation.
//
//nolint:gosec // G101 pattern-matches "PASSWORD"; this SQL contains no credential, only the GUC name one is read from at run time.
const setPasswordSQL = `
DO $$
DECLARE
    target    text := current_setting('provision.role');
    secret    text := current_setting('provision.password');
    is_super  boolean;
    is_bypass boolean;
BEGIN
    SELECT r.rolsuper, r.rolbypassrls
      INTO is_super, is_bypass
      FROM pg_catalog.pg_roles r
     WHERE r.rolname = target;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'role % does not exist', target
            USING HINT = 'Run "api migrate up" first: the migrations create the roles this command gives credentials to.';
    END IF;

    IF is_super OR is_bypass THEN
        RAISE EXCEPTION 'refusing to set a login password for %: rolsuper=%, rolbypassrls=%',
            target, is_super, is_bypass
            USING HINT = 'Row-level security is not enforced against that role, so a connection using this password would see every tenant. See docs/adr/0001-tenant-isolation.md.';
    END IF;

    EXECUTE format('ALTER ROLE %I WITH PASSWORD %L', target, secret);
END
$$`

// Roles applies every credential in one transaction, connecting with dsn.
//
// dsn must belong to the schema owner: setting another role's password needs
// ADMIN OPTION on it, which the owner has because the migrations created them.
// The serving role has no such right, which is the point — the process that
// serves traffic cannot rotate its own credential.
//
// One transaction for all of them, so a partial application is not a state
// anybody has to reason about.
func Roles(ctx context.Context, logger *slog.Logger, dsn string, credentials ...Credential) error {
	if len(credentials) == 0 {
		return fmt.Errorf("%w: nothing to provision", ErrInvalidCredential)
	}

	// Validated before a connection is opened, so a misconfiguration fails
	// immediately and names the offending value rather than surfacing as a
	// server error after a connection timeout.
	for _, credential := range credentials {
		if err := credential.Validate(); err != nil {
			return err
		}
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connecting to provision roles: %w", err)
	}

	defer func() {
		if cerr := conn.Close(ctx); cerr != nil {
			logger.Error("closing the provisioning connection", slog.Any("error", cerr))
		}
	}()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning the provisioning transaction: %w", err)
	}

	defer func() {
		// A no-op after a successful Commit. Present so that an error return
		// below cannot leave the transaction open on the way out.
		_ = tx.Rollback(ctx)
	}()

	for _, credential := range credentials {
		if err := setPassword(ctx, tx, credential); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing role credentials: %w", err)
	}

	for _, credential := range credentials {
		// The role name, never the secret. This line is the audit trail for
		// "when did this credential last change", so it is at info.
		logger.Info("database role password set from configuration",
			slog.String("role", credential.Role))
	}

	return nil
}

func setPassword(ctx context.Context, tx pgx.Tx, credential Credential) error {
	for _, setting := range []struct{ name, value string }{
		{"provision.role", credential.Role},
		{"provision.password", credential.Password},
	} {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, setting.name, setting.value); err != nil {
			return fmt.Errorf("staging %s: %w", setting.name, err)
		}
	}

	if _, err := tx.Exec(ctx, setPasswordSQL); err != nil {
		// The error text comes from a RAISE that names the role and the
		// attribute, and never the password.
		return fmt.Errorf("setting the password for role %s: %w", credential.Role, err)
	}

	return nil
}

// Describe renders the roles a call would touch, for a usage or dry-run
// message. It exists so that callers never have to build such a string out of
// a Credential and accidentally include the password.
func Describe(credentials []Credential) string {
	roles := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		roles = append(roles, credential.Role)
	}

	return strings.Join(roles, ", ")
}
