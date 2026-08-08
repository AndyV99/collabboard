package store

// The pre-tenant identity path — the second door, and the only other one.
//
// [Store.WithTenant] is the door for everything that happens inside a tenant.
// This is the door for the things that happen before there is one: login by
// email and password, "which organizations do I belong to", and creating a
// global user so that an invite has something to point at. See issue #13,
// docs/adr/0002-pre-tenant-identity-path.md, and migration 00004.
//
// Issue #8 added the credential queries — [IdentityQuerier.PasswordParams],
// [IdentityQuerier.VerifyPassword] and [IdentityQuerier.CreatePassword]. They
// travel this same Go door, but their SECURITY DEFINER functions are owned by a
// *different* database role from the four originals: collabboard_credentials,
// which holds column privileges on one table in a schema (auth) the serving
// role has no USAGE on, and no privilege of any kind in public. So the identity
// role's reach did not grow by one column to accommodate a password — a
// strictly narrower second role was created for it. See migration 00005 and
// docs/adr/0003-password-verifier-storage.md.
//
// # Why it cannot be a widened WithTenant
//
// users is global, and its policy is derived from memberships: a row is visible
// only if a membership joins it to the current tenant. Before login there is no
// tenant, so current_tenant_id() is NULL, so every one of those tables returns
// zero rows to the app role. That is the correct fail-closed behaviour, and it
// is also why these three operations are impossible rather than awkward.
//
// # Why it cannot become a general escape hatch
//
// Four things have to be true at once for a query to travel this path, and no
// single edit makes them all true:
//
//  1. The querier handed to the callback is [IdentityQuerier], generated from
//     identity_query.sql into a different package from the tenant-scoped
//     querier. The two share no methods, so q.ListProjects is a compile error
//     here — not a lint warning, not a review comment.
//  2. Every one of those methods is a call to a SECURITY DEFINER function
//     created in migration 00004 or 00005. The app role holds EXECUTE on
//     exactly those functions and can read nothing without a tenant otherwise.
//  3. Those functions run as one of two NOLOGIN roles, neither of which can
//     reach the other's data. collabboard_identity holds column-level
//     privileges on users, memberships and organizations, *no privileges at
//     all* on projects, boards, columns or cards, and nothing in the auth
//     schema — so it cannot read a password verifier. collabboard_credentials
//     holds column privileges on auth.user_credentials and nothing in public
//     at all — so it cannot read an email, a display name or a membership. A
//     function body that reached across would fail with "permission denied"
//     rather than returning the data.
//  4. Every call names an [IdentityReason] from a closed set and is logged.
//     A use that does not fit one of the reasons has nowhere to hide.
//
// So widening this path is not a matter of adding a Go method. It takes a
// migration that adds a function, a grant, possibly a privilege on a table the
// identity role has never had one on, and a new reason — each of which is a
// line in a diff that says what it is doing.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AndyV99/collabboard/apps/api/internal/store/internal/identitygen"
)

// The pre-tenant identity types, re-exported as aliases for the same reason the
// tenant-scoped ones are: callers need to name them without being able to
// import the package that constructs a querier. See types.go.
type (
	// IdentityQuerier is the complete pre-tenant surface: seven queries, each
	// one a call to a SECURITY DEFINER function. Callers never construct one;
	// [Store.WithoutTenant] hands it over.
	//
	// It shares no methods with [Querier]. That is the point — reaching a
	// tenant-scoped query from here does not compile.
	IdentityQuerier = identitygen.Querier

	// IdentityUser is the account [IdentityQuerier.FindUserByEmail] returns:
	// enough to identify who is logging in, and nothing about any organization
	// they belong to.
	IdentityUser = identitygen.FindUserByEmailRow

	// CreatedUser is the account [IdentityQuerier.CreateUser] returns.
	CreatedUser = identitygen.CreateUserRow

	// CreateUserParams are the arguments to [IdentityQuerier.CreateUser].
	CreateUserParams = identitygen.CreateUserParams

	// UserOrganization is one row of
	// [IdentityQuerier.ListUserOrganizations] — an organization the user
	// belongs to, and their role in it.
	UserOrganization = identitygen.ListUserOrganizationsRow

	// PasswordKDFParams are the argon2id parameters
	// [IdentityQuerier.PasswordParams] returns: a salt and a cost, both public
	// by construction. There is no verifier field, here or anywhere else — the
	// stored value never leaves the database. See ADR 0003.
	PasswordKDFParams = identitygen.PasswordParamsRow

	// VerifyPasswordParams are the arguments to
	// [IdentityQuerier.VerifyPassword]. Key is the raw argon2id output, which
	// the database hashes once more before comparing; it is not the password
	// and not the stored value.
	VerifyPasswordParams = identitygen.VerifyPasswordParams

	// CreatePasswordParams are the arguments to
	// [IdentityQuerier.CreatePassword].
	CreatePasswordParams = identitygen.CreatePasswordParams
)

// ErrNotFound is what a single-row query returns when there is no row.
//
// It is pgx.ErrNoRows under a name this package owns. internal/api and
// internal/realtime are forbidden by depguard from importing pgx at all, which
// is deliberate — but without this they would also be unable to tell "no such
// account" from "the database is down", and would end up importing the driver
// to find out. One alias is cheaper than that exception.
var ErrNotFound = pgx.ErrNoRows

// ErrNoIdentityReason means the caller passed the zero [IdentityReason].
//
// The zero value is constructible from outside this package — Go allows T{} for
// a struct with unexported fields — so it is rejected here rather than being
// allowed to open the pre-tenant path under a blank reason that no audit log
// could account for.
var ErrNoIdentityReason = errors.New("store: a pre-tenant identity reason is required")

// clearTenantSQL empties app.tenant_id for the pre-tenant transaction.
//
// Belt and braces: SET LOCAL is already reverted at commit or rollback, so a
// pooled connection should never arrive carrying one. But "should never" is the
// assumption this whole package exists to stop relying on, and a pre-tenant
// transaction that silently inherited a tenant would make a login lookup behave
// differently depending on which request ran before it. Cheaper to guarantee it
// than to reason about it.
const clearTenantSQL = `SELECT set_config('app.tenant_id', '', true)`

// IdentityReason is why the pre-tenant path is being opened.
//
// A struct with one unexported field rather than a string, so the set of
// reasons is closed: the constants below are the only values any package can
// name, and a caller who wants another has to add it here, next to the existing
// ones and next to the queries they justify. That is the same friction as
// adding an alias to types.go, and it exists for the same reason.
type IdentityReason struct{ name string }

// String returns the reason as it appears in logs.
func (r IdentityReason) String() string {
	if r.name == "" {
		return "unspecified"
	}

	return r.name
}

// The reasons the pre-tenant path exists. Each one maps to an operation
// that cannot run inside a tenant-scoped transaction — see identity_query.sql
// for the justification per query.
var (
	// ReasonLogin: resolving an account by email during authentication, when
	// no organization has been claimed yet.
	ReasonLogin = IdentityReason{name: "login"}

	// ReasonListOrganizations: populating the organization switcher after
	// login. The answer spans tenants, so it cannot be asked from inside one.
	ReasonListOrganizations = IdentityReason{name: "list_organizations"}

	// ReasonInviteLookup: resolving an invited email to an existing account,
	// which by definition lives outside the inviting tenant's visibility.
	ReasonInviteLookup = IdentityReason{name: "invite_lookup"}

	// ReasonRegisterUser: creating a global identity, together with the
	// credential that identity authenticates with. Both halves run in one
	// transaction, so a user row can never be committed without the password
	// that makes it usable.
	ReasonRegisterUser = IdentityReason{name: "register_user"}

	// ReasonPasswordParams: reading the argon2id parameters for a login, which
	// happens before any organization has been claimed. Separate from
	// ReasonVerifyPassword because it is a separate transaction — the ~80ms
	// derivation happens between the two, and holding a pooled connection open
	// across it would let a handful of concurrent logins exhaust the pool.
	ReasonPasswordParams = IdentityReason{name: "password_params"}

	// ReasonVerifyPassword: comparing a derived key against the stored
	// verifier. Pre-tenant for the same reason the lookup that preceded it is.
	ReasonVerifyPassword = IdentityReason{name: "verify_password"}
)

// IdentityFunc runs inside the pre-tenant transaction. The querier it receives
// is bound to that transaction and must not outlive it.
//
// Returning a non-nil error rolls the transaction back.
type IdentityFunc func(ctx context.Context, q IdentityQuerier) error

// WithoutTenant opens the pre-tenant identity path.
//
// It acquires a pooled connection, begins a transaction, explicitly clears
// app.tenant_id, and runs fn against an [IdentityQuerier]. The transaction
// commits when fn returns nil and rolls back otherwise, including on a panic;
// the connection returns to the pool on every path.
//
// reason is not decoration. It is required, it is logged at every call, and it
// is drawn from a closed set — so "who uses this and why" is answerable from
// the logs of a running service as well as from a grep of the source.
//
// Use [Store.WithTenant] for anything a tenant could do. This is for the three
// things no tenant can.
func (s *Store) WithoutTenant(ctx context.Context, reason IdentityReason, fn IdentityFunc) (err error) {
	if s == nil || s.pool == nil {
		return ErrNilPool
	}

	if fn == nil {
		return ErrNilFunc
	}

	if reason == (IdentityReason{}) {
		return ErrNoIdentityReason
	}

	started := time.Now()

	// Logged on entry rather than on success, so an attempt that panics or
	// times out still leaves a record. No email, no user id and no display
	// name: the audit question is "was the pre-tenant path used, and for
	// what", and answering it does not require logging who was looked up.
	slog.InfoContext(ctx, "pre-tenant identity path opened", slog.String("reason", reason.name))

	defer func() {
		slog.InfoContext(ctx, "pre-tenant identity path closed",
			slog.String("reason", reason.name),
			slog.Duration("duration", time.Since(started)),
			slog.Bool("ok", err == nil),
		)
	}()

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}

	// Registered before the rollback defer so that it runs after it: pgxpool
	// destroys, rather than reuses, a connection released while its transaction
	// is still open.
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}

	committed := false

	defer func() {
		if committed {
			return
		}

		// Detached from ctx for the same reason WithTenant's rollback is: a
		// rollback inheriting a cancelled context fails instantly, leaves the
		// transaction open at release time, and makes pgxpool throw the
		// connection away.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		if rerr := tx.Rollback(rollbackCtx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rolling back pre-tenant transaction: %w", rerr))
		}
	}()

	if _, err := tx.Exec(ctx, clearTenantSQL); err != nil {
		return fmt.Errorf("clearing tenant context: %w", err)
	}

	if err := fn(ctx, identitygen.New(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing pre-tenant transaction: %w", err)
	}

	committed = true

	return nil
}

// WithoutTenantValue is [Store.WithoutTenant] for a callback that produces a
// value, exactly as [InTenant] is for [Store.WithTenant]. Every one of the
// pre-tenant operations returns something, so without this each call site would
// declare a variable outside a closure just to smuggle the result out.
//
// It is a function rather than a method because Go methods cannot take type
// parameters.
//
// On error the zero value is returned even if fn itself succeeded: a commit can
// fail after fn has produced a value, and handing back a user id from a
// transaction that did not commit is precisely the quiet wrongness this package
// exists to prevent.
func WithoutTenantValue[T any](ctx context.Context, s *Store, reason IdentityReason, fn func(ctx context.Context, q IdentityQuerier) (T, error)) (T, error) {
	var out T

	err := s.WithoutTenant(ctx, reason, func(ctx context.Context, q IdentityQuerier) error {
		var ferr error

		out, ferr = fn(ctx, q)

		return ferr
	})
	if err != nil {
		var zero T

		return zero, err
	}

	return out, nil
}
