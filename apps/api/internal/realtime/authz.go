package realtime

// Subscription authorization.
//
// # Why this file is the security-critical one
//
// The upgrade is authenticated by the same middleware as every REST route, and
// the tenant comes from the same verified claim, so the BOLA class of bug that
// auth_bola_test.go closed for HTTP is closed here too — a client cannot name
// an organization.
//
// What a client *can* name is a board id, in a subscribe frame. So the
// WebSocket surface reintroduces exactly one instance of the object-level
// problem, and this is where it is answered. The tempting non-answer is to
// check that the id parses as a uuid and register the subscription: the room
// key contains the tenant, the RLS-backed queries elsewhere return nothing for
// a foreign board, so what harm? The harm is that the room key would be built
// from a client-supplied board id under the *caller's own* tenant, and the
// publisher on the other instance builds the same key from its own principal.
// Two organizations that happened to name the same board id — a copied id, a
// restored backup, a uuid pasted from a support ticket — would share a room.
// bola_test.go builds that authorizer on purpose and shows the leak.
//
// # What "authorized" means here
//
// Two conditions, both inside one tenant-scoped transaction:
//
//  1. The subject still has a membership in the tenant the token names. This is
//     the check that makes revocation take effect on a live connection: the
//     token cannot be un-issued, but a membership can disappear, and conn.go
//     re-runs this on an interval for every live subscription.
//
//  2. The board resolves inside that tenant. This is RLS doing the work — the
//     query carries no tenant predicate, the policy supplies it, and a board
//     belonging to somebody else comes back as no row rather than as a row.
//
// Note what the schema does and does not offer. Membership is per organization,
// not per board (migrations/00002_tenancy.sql), so "a board in your own tenant
// you have no membership for" is not a state this data model can be in. When
// per-board ACLs exist, this function is the one place that has to learn about
// them, which is the reason it takes the whole principal rather than a tenant.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// ErrForbidden means the principal may not watch that board. It is returned
// for a board in another tenant, a board that does not exist, and a subject
// whose membership has been revoked — deliberately the same error for all
// three, so a client cannot use the WebSocket as a board-existence oracle for
// organizations it does not belong to.
var ErrForbidden = errors.New("realtime: not authorized for that board")

// Authorizer decides whether a principal may subscribe to a board.
//
// An interface so the hub's concurrency can be tested without a database, and
// so bola_test.go can substitute the vulnerable implementation it needs to
// prove the assertions have teeth. The production implementation is
// [StoreAuthorizer] and there is no other.
type Authorizer interface {
	// AuthorizeBoard returns nil if principal may watch boardID, [ErrForbidden]
	// if it may not, and any other error if the question could not be answered
	// — which callers must treat as a refusal, not as permission.
	AuthorizeBoard(ctx context.Context, principal auth.Principal, boardID uuid.UUID) error

	// AuthorizeTenant returns nil while principal is still a member of the
	// organization its token names, and [ErrForbidden] once it is not.
	//
	// Separate from AuthorizeBoard because it is the question a live connection
	// asks about itself rather than about a board: a connection watching no
	// boards still has to find out that its membership was revoked, and the
	// answer decides whether the socket stays open at all. AuthorizeBoard
	// checks membership too — it has to, since it is the front door — so this
	// is a narrower question and not a subset that could be skipped there.
	AuthorizeTenant(ctx context.Context, principal auth.Principal) error
}

// TenantStore is the slice of internal/store this package uses.
//
// One method, and it is the one that takes a tenant id explicitly. Same
// reasoning as internal/api's TenantStore: passing it rather than reading it
// from a context means every call site has to name where the tenant came from,
// and the single call site below names a principal.
type TenantStore interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error
}

// StoreAuthorizer answers the question from Postgres, under row-level
// security.
type StoreAuthorizer struct {
	store TenantStore
}

// NewStoreAuthorizer wraps a store.
func NewStoreAuthorizer(tenantStore TenantStore) *StoreAuthorizer {
	return &StoreAuthorizer{store: tenantStore}
}

// AuthorizeBoard implements [Authorizer].
//
// One transaction for both checks, so they cannot disagree with each other: a
// membership revoked between two round trips would otherwise leave a window
// where the subject is a member for the purposes of the first query and not the
// second.
func (a *StoreAuthorizer) AuthorizeBoard(ctx context.Context, principal auth.Principal, boardID uuid.UUID) error {
	// Checked before the database, because uuid.Nil is a syntactically valid id
	// that matches no row: passing it through would produce a refusal for the
	// right reason by accident, and would stop doing so the day a row with a
	// zero id existed.
	if principal.TenantID == uuid.Nil || principal.UserID == uuid.Nil || boardID == uuid.Nil {
		return ErrForbidden
	}

	// principal.TenantID, and there is no other expression that could appear
	// here. Grep this package for WithTenant: this is the only call.
	return a.store.WithTenant(ctx, principal.TenantID, func(ctx context.Context, q store.Querier) error {
		if err := requireMembership(ctx, q, principal); err != nil {
			return err
		}

		if _, err := q.GetBoard(ctx, boardID); err != nil {
			if errors.Is(err, store.ErrNoRows) {
				return ErrForbidden
			}

			return fmt.Errorf("reading the board: %w", err)
		}

		return nil
	})
}

// AuthorizeTenant implements [Authorizer].
func (a *StoreAuthorizer) AuthorizeTenant(ctx context.Context, principal auth.Principal) error {
	if principal.TenantID == uuid.Nil || principal.UserID == uuid.Nil {
		return ErrForbidden
	}

	return a.store.WithTenant(ctx, principal.TenantID, func(ctx context.Context, q store.Querier) error {
		return requireMembership(ctx, q, principal)
	})
}

// requireMembership is the revocation check.
//
// The query carries no tenant predicate: the policy on memberships supplies
// tenant_id = current_tenant_id(), so this asks "is this user a member of the
// transaction's tenant" and cannot accidentally ask anything wider. No row
// means either never a member or no longer one, which are the same answer here.
func requireMembership(ctx context.Context, q store.Querier, principal auth.Principal) error {
	if _, err := q.GetMembership(ctx, principal.UserID); err != nil {
		if errors.Is(err, store.ErrNoRows) {
			return ErrForbidden
		}

		return fmt.Errorf("reading the subscriber's membership: %w", err)
	}

	return nil
}
