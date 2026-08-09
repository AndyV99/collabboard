package auth

// What GET /me reports: who the caller is, and where they can act (issue #75).
//
// # Why this is one method rather than two
//
// Before this, /me could name the caller's *organizations* but not the caller.
// The web shell therefore called GET /members and looked for itself in the list
// — O(members) work and a read of every colleague's address, to render one
// name. [Service.Me] answers both halves in one call so the client spends one
// request, and it reads the caller's own row directly so the cost is one row
// rather than the whole organization.
//
// # Where the two halves come from, and why they cannot share a transaction
//
//   - the organizations span tenants, so the question cannot be asked with one
//     current. It goes through the pre-tenant door (ADR 0002), under
//     ReasonListOrganizations, exactly as it did before this change;
//   - the caller's own `users` row is visible *with* a tenant current, because
//     users_visible_via_membership makes a row visible when a membership joins
//     it to the current tenant — and the caller is by definition a member of
//     the tenant in their own token. So it is an ordinary tenant-scoped read
//     through [store.Store.WithTenant], the same door GET /members uses.
//
// The second half is deliberately *not* a new pre-tenant capability. That door
// exists for operations that genuinely have no tenant, and /me is called with
// one in context; widening it here would have taken a migration, a grant and a
// fifth reason to do something the ordinary door already does.
//
// Two doors means two transactions, which is a property of the design rather
// than of this function — see internal/store. It is one round trip from the
// client, which is the cost the issue was about, and one extra primary-key read
// against the request that replaces it.
//
// # What it discloses
//
// The caller's own email and display name, to the caller, over an authenticated
// request. GET /members already returns both to any member of the organization,
// so this is strictly narrower than what the same token could already fetch.
//
// It cannot be pointed at anyone else. The user id passed to GetUser is
// [Principal.UserID], read from the verified access token; there is no request
// field anywhere in this service that carries a user id, and the policy would
// still refuse an id from outside the caller's organization. Asserted in
// internal/api/auth_bola_test.go and end to end in
// internal/api/me_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// UserProfile is an account as its owner sees it: who they are, with nothing
// about what they can do. The role and the organization belong to a membership,
// not to the identity, and they travel in [MeResult] alongside this.
type UserProfile struct {
	UserID      uuid.UUID
	Email       string
	DisplayName string
}

// MeResult is the authenticated caller: their own identity, plus every
// organization they could act in.
type MeResult struct {
	Profile       UserProfile
	Organizations []Organization
}

// Me reports the caller's own identity and memberships.
//
// The subject is [Principal.UserID] and nothing else, for both halves. Neither
// underlying query authorizes on its own — ListUserOrganizations does not (ADR
// 0002 says so explicitly) and GetUser authorizes only as far as the current
// tenant — so an id taken from a request would turn this into a directory
// endpoint. There is nowhere for one to arrive from; see me.go's header.
func (s *Service) Me(ctx context.Context, principal Principal) (MeResult, error) {
	organizations, err := s.organizations(ctx, principal.UserID)
	if err != nil {
		return MeResult{}, err
	}

	profile, err := s.profile(ctx, principal)
	if err != nil {
		return MeResult{}, err
	}

	return MeResult{Profile: profile, Organizations: organizations}, nil
}

// profile reads the caller's own users row in their own tenant context.
//
// No row means the membership that made the row visible is gone — the token
// still claims it, but the token was minted up to an access-token lifetime ago
// and the database is the current answer. That is [ErrNotAMember], the same
// answer AddMember gives a caller whose membership has been revoked, and a 403
// rather than a 500: nothing has failed, the caller is simply no longer
// entitled. Refresh will revoke the session at the next rotation.
func (s *Service) profile(ctx context.Context, principal Principal) (UserProfile, error) {
	var row store.GetUserRow

	err := s.store.WithTenant(ctx, principal.TenantID, func(ctx context.Context, q store.Querier) error {
		var qerr error

		row, qerr = q.GetUser(ctx, principal.UserID)

		return qerr
	})

	switch {
	case errors.Is(err, store.ErrNoRows):
		s.logger.WarnContext(ctx, "identity lookup found no row for the caller",
			slog.String("event", "auth.me.membership_revoked"),
			slog.String("user_id", principal.UserID.String()),
			slog.String("tenant_id", principal.TenantID.String()),
		)

		return UserProfile{}, ErrNotAMember
	case err != nil:
		return UserProfile{}, fmt.Errorf("reading the caller's own account: %w", err)
	case row.ID != principal.UserID:
		// Unreachable through the SQL — the query filters on the id it is given
		// — but this is the endpoint whose entire job is to tell someone who
		// they are, and answering with a different account would be the worst
		// possible way to be wrong. Refused rather than trusted.
		s.logger.ErrorContext(ctx, "identity lookup returned a different account",
			slog.String("event", "auth.me.identity_mismatch"),
			slog.String("user_id", principal.UserID.String()),
			slog.String("tenant_id", principal.TenantID.String()),
		)

		return UserProfile{}, errors.New("auth: identity lookup returned a different account")
	}

	return UserProfile{UserID: row.ID, Email: row.Email, DisplayName: row.DisplayName}, nil
}
