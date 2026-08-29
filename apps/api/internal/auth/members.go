package auth

// Adding an existing account to an organization — the demo's "invite teammate"
// step (issue #61).
//
// # Direct add, not an invitation
//
// This adds the account to the organization immediately. It does not create an
// invitation the invitee accepts. The reasoning is in
// docs/adr/0008-organization-membership-by-direct-add.md; the two constraints
// that decided it are that an accept step needs the invitee to read and act on
// a row belonging to a tenant they are not yet in — which is a *pre-tenant*
// operation, and ADR 0002's door is deliberately not wide enough for one — and
// that delivering an invitation needs a mailer this service does not have.
//
// What is done instead of consent-by-acceptance:
//
//   - only an account that already exists can be added. This path never creates
//     a user, so an address with no account cannot be pulled into an
//     organization at all;
//   - being added grants a membership and nothing else. A member cannot add
//     anyone, so an addition cannot cascade;
//   - every attempt is logged with the actor, the tenant and the outcome.
//
// # Who may add
//
// owner and admin. A member may not — the issue is explicit that "any member"
// is not an acceptable default, and a member is the role every added account
// gets, so allowing it would make one addition enough to grow the organization
// without limit.
//
// The role granted is bounded by the actor's own: an owner may add a member or
// an admin, an admin may add a member only, and nobody may add an owner through
// this path. Transferring ownership is a different operation with different
// consequences and it is not this one.
//
// # What this discloses
//
// The caller learns one bit: whether the address they typed has an account.
// That bit is inherent — the operation succeeds only if the account exists —
// and it is bounded rather than eliminated:
//
//   - the asker must be authenticated and must be an owner or admin, so the
//     lookup is not reachable anonymously or by a plain member;
//   - authorization is decided *before* the directory is consulted, so a caller
//     who may not add never causes a lookup at all;
//   - the lookup is one exact address per request. The pre-tenant door exposes
//     no pattern query and no query returning a set of users, and adding one
//     would take a migration (ADR 0002);
//   - the resolved id is used and never returned on a failure path. A refusal
//     carries a fixed sentence and no user data — no id, no display name, no
//     other organization the account belongs to;
//   - every attempt is logged with actor, tenant and outcome, and never with
//     the address, so bulk probing is visible in the logs without the logs
//     becoming the address list.
//
// POST /api/v1/auth/register already discloses the same bit to an
// *unauthenticated* caller — a duplicate address is a 409, accepted
// deliberately in auth.go — so this endpoint is not the widest way to ask the
// question.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// The membership roles, matching the CHECK constraint in migration 00002.
//
// [RoleOwner] is declared in service.go, next to the registration that grants
// it.
const (
	// RoleAdmin may add members. It may not add another admin: only an owner
	// can, which is what stops an addition from being a way to manufacture
	// peers.
	RoleAdmin = "admin"

	// RoleMember is the default an added account gets, and the role that may
	// not add anyone.
	RoleMember = "member"
)

// The errors AddMember distinguishes. Everything else is wrapped and reported
// as a server failure.
var (
	// ErrInsufficientRole means the caller is a member of the organization but
	// their role does not permit the addition they asked for. Distinct from
	// [ErrNotAMember], which is about belonging rather than about privilege.
	ErrInsufficientRole = errors.New("auth: role does not permit adding a member")

	// ErrNoSuchAccount means the address has no account. It is deliberately
	// *not* folded into ErrInvalidInput: the HTTP layer answers it with a fixed
	// sentence carrying no user data, and conflating it with a validation error
	// would make a well-formed address that happens to be unregistered look
	// like a malformed one.
	ErrNoSuchAccount = errors.New("auth: no account with that address")

	// ErrAlreadyMember means the account is already in this organization. The
	// caller can already see that from GET /api/v1/members, so saying so
	// discloses nothing they were not entitled to.
	ErrAlreadyMember = errors.New("auth: already a member of that organization")
)

// AddMemberInput is a request to put an existing account into the principal's
// organization.
//
// There is no organization field, and there is deliberately nowhere to put one.
// The tenant is [Principal.TenantID] — the verified org claim — exactly as it
// is for every other tenant-scoped operation in the service.
type AddMemberInput struct {
	// Principal is the authenticated caller. Both the tenant the addition lands
	// in and the role the addition is authorized against come from it.
	Principal Principal

	// Email is the address to add. Normalised before use; matched exactly, not
	// by pattern.
	Email string

	// Role is the membership role to grant. Empty means [RoleMember].
	Role string
}

// AddMemberResult is the membership that was created.
//
// It carries no display name and no information about any other organization
// the account belongs to. Email is the normalised address the caller submitted
// rather than anything read back from the directory, so a successful response
// contains nothing the caller did not already have or create.
type AddMemberResult struct {
	MembershipID uuid.UUID
	UserID       uuid.UUID
	Email        string
	Role         string
	JoinedAt     time.Time
}

// AddMember puts an existing account into the caller's organization.
//
// # The three steps, in this order and for this reason
//
//  1. **Authorize**, in a transaction scoped to the caller's own tenant. The
//     caller's role is read from `memberships` rather than from the token's
//     role claim: the claim is minted at login and refreshed at most once per
//     access-token lifetime, so a demoted admin would keep the claim for the
//     rest of it. The database is the current answer.
//  2. **Resolve** the address to a user id, through the pre-tenant door. This
//     is the one operation here that a tenant-scoped transaction cannot do —
//     the account may live entirely outside the caller's visibility, which is
//     what `ResolveUserIDByEmail` was built for (ADR 0002). It runs only after
//     step 1 has said yes, so a caller who may not add never reaches the
//     directory.
//  3. **Insert**, in a second tenant-scoped transaction that re-runs the check
//     from step 1 before writing. The re-check is not belt and braces for its
//     own sake: it is what makes "the actor held the role at the instant the
//     row was written" true rather than "held it a moment earlier".
//
// Three transactions rather than one because the middle step travels the other
// door, and the two doors are separate transactions by construction. See
// internal/store.
//
// Duplicate additions are refused by the unique index on
// (tenant_id, user_id) — the INSERT is attempted and 23505 becomes
// [ErrAlreadyMember]. Reading first and inserting second would leave a window
// where two concurrent requests both see no row; the index has no such window.
func (s *Service) AddMember(ctx context.Context, in AddMemberInput) (AddMemberResult, error) {
	email := NormalizeEmail(in.Email)

	role, err := validateAddMember(email, in.Role)
	if err != nil {
		return AddMemberResult{}, err
	}

	// Step 1: authorize, before anything is looked up.
	if err := s.store.WithTenant(ctx, in.Principal.TenantID, func(ctx context.Context, q store.Querier) error {
		return authorizeAddMember(ctx, q, in.Principal.UserID, role)
	}); err != nil {
		if isAddMemberRefusal(err) {
			return AddMemberResult{}, s.refuseAddMember(ctx, in.Principal, err)
		}

		return AddMemberResult{}, fmt.Errorf("authorizing the addition: %w", err)
	}

	// Step 2: the directory lookup, through the narrow pre-tenant door and
	// under the reason that door was built for.
	userID, err := withoutTenant(ctx, s.store, store.ReasonInviteLookup,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.ResolveUserIDByEmail(ctx, email)
		})

	switch {
	case errors.Is(err, store.ErrNotFound):
		return AddMemberResult{}, s.refuseAddMember(ctx, in.Principal, ErrNoSuchAccount)
	case err != nil:
		return AddMemberResult{}, fmt.Errorf("resolving the address: %w", err)
	case userID == uuid.Nil:
		// Cannot happen through the SQL — the function returns a row or no row
		// — but a nil id would become a foreign-key violation surfacing as a
		// 500, and refusing is both cheaper and more honest.
		return AddMemberResult{}, s.refuseAddMember(ctx, in.Principal, ErrNoSuchAccount)
	}

	// Step 3: the write, re-authorized in the transaction that performs it.
	var membership store.Membership

	err = s.store.WithTenant(ctx, in.Principal.TenantID, func(ctx context.Context, q store.Querier) error {
		if aerr := authorizeAddMember(ctx, q, in.Principal.UserID, role); aerr != nil {
			return aerr
		}

		created, cerr := q.CreateMembership(ctx, store.CreateMembershipParams{UserID: userID, Role: role})
		if cerr != nil {
			return cerr
		}

		membership = created

		return nil
	})

	switch {
	case err != nil && store.IsUniqueViolation(err):
		return AddMemberResult{}, s.refuseAddMember(ctx, in.Principal, ErrAlreadyMember)
	case isAddMemberRefusal(err):
		return AddMemberResult{}, s.refuseAddMember(ctx, in.Principal, err)
	case err != nil:
		return AddMemberResult{}, fmt.Errorf("creating the membership: %w", err)
	}

	// No email and no display name in this line, for the same reason
	// logFailedLogin carries neither: the operational question is who added
	// whom to what, and the user id answers it without turning the log into an
	// address book.
	s.logger.InfoContext(ctx, "member added",
		slog.String("event", "auth.member.added"),
		slog.String("actor_user_id", in.Principal.UserID.String()),
		slog.String("tenant_id", in.Principal.TenantID.String()),
		slog.String("user_id", membership.UserID.String()),
		slog.String("role", membership.Role),
	)

	return AddMemberResult{
		MembershipID: membership.ID,
		UserID:       membership.UserID,
		Email:        email,
		Role:         membership.Role,
		JoinedAt:     membership.CreatedAt,
	}, nil
}

// authorizeAddMember decides whether one member of the current tenant may add
// another with the given role.
//
// It takes the querier rather than opening its own transaction so that the
// caller decides which transaction the decision belongs to — which for the
// write is the same one as the INSERT.
//
// The membership is read from the current tenant's rows: the policy on
// `memberships` supplies `tenant_id = current_tenant_id()`, so a caller holding
// a token for an organization they have been removed from finds no row and is
// refused. That is [ErrNotAMember] rather than a 500.
func authorizeAddMember(ctx context.Context, q store.Querier, actorID uuid.UUID, granting string) error {
	membership, err := q.GetMembership(ctx, actorID)
	if errors.Is(err, store.ErrNoRows) {
		return ErrNotAMember
	} else if err != nil {
		return fmt.Errorf("reading the caller's membership: %w", err)
	}

	switch membership.Role {
	case RoleOwner:
		// An owner may grant member or admin. Granting owner is refused by
		// validateAddMember before this is reached.
		return nil
	case RoleAdmin:
		if granting == RoleAdmin {
			return ErrInsufficientRole
		}

		return nil
	default:
		return ErrInsufficientRole
	}
}

// validateAddMember checks the submitted address and role, and returns the role
// to grant.
//
// Refusing RoleOwner here rather than in authorizeAddMember is deliberate: it
// is a property of this endpoint, not of the caller. No role can grant it, so
// no owner should be able to discover that theirs almost could.
func validateAddMember(email, role string) (string, error) {
	// Shared with registration rather than restated, so the two cannot drift.
	// This path does not INSERT a user -- ADR 0008 adds an existing account to
	// an organization -- so a malformed address here answers "not found" rather
	// than 500ing on the column constraint. That makes the looseness harmless
	// today and is exactly why it would have survived unnoticed.
	if err := validateEmailAddress(email); err != nil {
		return "", err
	}

	switch role {
	case "":
		return RoleMember, nil
	case RoleMember, RoleAdmin:
		return role, nil
	case RoleOwner:
		return "", fmt.Errorf("%w: role must be %q or %q; ownership is not transferable through this endpoint",
			ErrInvalidInput, RoleMember, RoleAdmin)
	default:
		return "", fmt.Errorf("%w: role must be %q or %q", ErrInvalidInput, RoleMember, RoleAdmin)
	}
}

// refuseAddMember logs a refusal and returns the error unchanged.
//
// One log line for every refusal, whatever the reason, carrying the actor, the
// tenant and a stable outcome label — and never the address that was probed.
// That is what makes a burst of "no_such_account" from one actor visible to an
// operator without the log itself becoming the enumeration result.
func (s *Service) refuseAddMember(ctx context.Context, principal Principal, err error) error {
	s.logger.WarnContext(ctx, "adding a member refused",
		slog.String("event", "auth.member.add_refused"),
		slog.String("reason", addMemberRefusal(err)),
		slog.String("actor_user_id", principal.UserID.String()),
		slog.String("tenant_id", principal.TenantID.String()),
	)

	return err
}

// isAddMemberRefusal reports whether err is one of the authorization outcomes
// this operation answers with a 4xx, as opposed to a failure of the database or
// of the wiring — which must become a 500 with the detail in the log, never a
// refusal a client could read as "you are not allowed".
func isAddMemberRefusal(err error) bool {
	return errors.Is(err, ErrNotAMember) || errors.Is(err, ErrInsufficientRole)
}

// addMemberRefusal maps a refusal to a stable label for logs and metrics.
// A closed set, because it ends up in a log field and must not be able to carry
// attacker-controlled text.
func addMemberRefusal(err error) string {
	switch {
	case errors.Is(err, ErrNotAMember):
		return "not_a_member"
	case errors.Is(err, ErrInsufficientRole):
		return "insufficient_role"
	case errors.Is(err, ErrNoSuchAccount):
		return "no_such_account"
	case errors.Is(err, ErrAlreadyMember):
		return "already_a_member"
	default:
		return "error"
	}
}
