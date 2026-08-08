// Package store is the Postgres data layer, and the only place in the service
// that talks to tenant-scoped tables.
//
// # The rule
//
// Every tenant-scoped query runs inside a transaction that has already set
// app.tenant_id, and the only way to open such a transaction is [Store.WithTenant].
// Nothing else in the service holds a *pgxpool.Pool, and nothing else can
// construct a querier: the sqlc output lives in internal/store/internal/gen,
// which Go's internal-package rule makes importable only from within
// internal/store. A handler that wants to run SQL has exactly one door.
//
// # Why
//
// Tenant isolation is enforced by Postgres row-level security rather than by
// application predicates — see docs/adr/0001-tenant-isolation.md. Every policy
// is written against current_tenant_id(), which reads the app.tenant_id GUC and
// returns NULL when it is unset, so a transaction that never set it sees zero
// rows rather than every row. That is a good failure mode, but it is only a
// failure mode: the isolation is real only while every query actually runs
// inside a transaction that set the GUC. This package is that guarantee, which
// is why it is worth the ceremony of a nested internal package.
//
// SET LOCAL, not SET, is the load-bearing detail. SET LOCAL is reset at commit
// or rollback, so a connection handed back to the pool carries no tenant state
// and cannot serve the next request with the previous request's tenant.
//
// # What this package deliberately cannot do
//
// Identity operations that happen before a tenant is known — login by email,
// "which organizations do I belong to", inviting a user who already has an
// account elsewhere — cannot run through WithTenant, because there is no tenant
// yet and the users policy is derived from memberships. That path is issue #13
// and belongs in a separate, named, auditable entry point on [Store]; it is not
// a reason to widen this one.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/store/internal/gen"
)

// Errors returned by [Store.WithTenant] before it touches the database.
var (
	// ErrNilPool means the Store was built without a pool, which is a wiring
	// bug rather than a runtime condition.
	ErrNilPool = errors.New("store: no connection pool")

	// ErrNoTenant means the caller passed the zero uuid. It is reported rather
	// than executed because uuid.Nil is a syntactically valid tenant that
	// matches no organization: running with it would silently return empty
	// results, which reads like "no data" instead of "no tenant".
	ErrNoTenant = errors.New("store: tenant id is required")

	// ErrNilFunc means the caller passed a nil callback.
	ErrNilFunc = errors.New("store: callback is required")
)

// setTenantSQL scopes the transaction to one tenant.
//
// SET LOCAL takes a literal and cannot be parameterised, and interpolating a
// value into DDL-ish SQL is how injection happens. set_config is the
// parameterised equivalent: its third argument, is_local, gives exactly SET
// LOCAL semantics — the setting is reverted at commit or rollback — while the
// value travels as a bind parameter.
const setTenantSQL = `SELECT set_config('app.tenant_id', $1, true)`

// rollbackTimeout bounds the rollback that runs after a failed callback, which
// deliberately does not inherit the caller's context. See WithTenant.
const rollbackTimeout = 5 * time.Second

// TenantFunc runs inside a tenant-scoped transaction. The querier it receives is
// bound to that transaction and must not outlive it: retaining it and calling it
// after WithTenant returns will fail against a closed transaction rather than
// silently run without tenant context.
//
// Returning a non-nil error rolls the transaction back.
type TenantFunc func(ctx context.Context, q Querier) error

// Store owns the connection pool. It is the only type in the service that does,
// and it exposes no accessor for it, so there is no supported way to obtain a
// pool-bound querier.
type Store struct {
	pool *pgxpool.Pool
}

// New wraps a pool. The pool must be configured with the serving role
// (collabboard_app), which is not a superuser and holds no BYPASSRLS — the
// policies are decorative against any other identity.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// WithTenant acquires a pooled connection, opens a transaction, scopes it to
// tenantID, and runs fn against a querier bound to that transaction.
//
// The transaction commits when fn returns nil and rolls back otherwise,
// including when fn panics. The connection is returned to the pool on every
// path.
//
// A rollback failure is joined to fn's error rather than replacing it, because
// the reason the work failed is more useful than the fact that the cleanup also
// did.
func (s *Store) WithTenant(ctx context.Context, tenantID uuid.UUID, fn TenantFunc) (err error) {
	if s == nil || s.pool == nil {
		return ErrNilPool
	}

	if fn == nil {
		return ErrNilFunc
	}

	if tenantID == uuid.Nil {
		return ErrNoTenant
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}

	// Registered before the rollback defer so that it runs after it: pgxpool
	// destroys, rather than reuses, a connection released while its transaction
	// is still open, so the pool only gets the connection back if the rollback
	// happens first.
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

		// Detached from ctx on purpose. The common reason fn fails is that ctx
		// was cancelled — a client that hung up — and a rollback inheriting a
		// cancelled context fails instantly, which leaves the transaction open
		// at release time and makes pgxpool throw the connection away. Under
		// load-shedding that turns every abandoned request into pool churn.
		// Bounded, so a wedged server cannot make this the thing that hangs.
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		// pgx.ErrTxClosed means something already finished the transaction —
		// most often a failed Commit, which rolls back on its own. That is the
		// expected state here, not an error worth reporting.
		if rerr := tx.Rollback(rollbackCtx); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rolling back tenant transaction: %w", rerr))
		}
	}()

	if _, err := tx.Exec(ctx, setTenantSQL, tenantID.String()); err != nil {
		return fmt.Errorf("setting tenant context: %w", err)
	}

	// Returned unwrapped: the caller wrote fn and knows what it was doing, and
	// wrapping here would only add a layer between them and their own sentinel.
	if err := fn(ctx, gen.New(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing tenant transaction: %w", err)
	}

	committed = true

	return nil
}

// InTenant is [Store.WithTenant] for the common case of a callback that produces
// a value. It exists so that reading one row does not require declaring a
// variable outside a closure just to smuggle the result out — the safe path
// should also be the pleasant one, or callers will look for another.
//
// It is a function rather than a method because Go methods cannot take type
// parameters.
//
// On error the zero value is returned even if fn itself succeeded: a commit can
// fail after fn has already produced a value, and handing back rows from a
// transaction that did not commit is exactly the kind of quiet wrongness this
// package exists to prevent.
func InTenant[T any](ctx context.Context, s *Store, tenantID uuid.UUID, fn func(ctx context.Context, q Querier) (T, error)) (T, error) {
	var out T

	err := s.WithTenant(ctx, tenantID, func(ctx context.Context, q Querier) error {
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
