package auth

// Creating the organization a half-registered account never got (issue #34).
//
// # The state this exists for
//
// Registration is two transactions and structurally must be: a pre-tenant one
// creating the global user and its credential, and a tenant-scoped one creating
// the organization and the owner membership. The tenant does not exist until the
// second one's first statement runs, and the two doors are separate transactions
// by construction — see internal/store and ADR 0002. A failure between them (a
// connection drop, a pod eviction, a Postgres failover) commits the first and
// loses the second, leaving an account that authenticates fine and belongs to no
// organization.
//
// Before this, that account was stuck. The address is taken, so re-registering
// returns 409; nothing created an organization for an existing account; and the
// pre-tenant path deliberately has no delete, so it could not be cleaned up
// either. The only exit was an operator with a psql prompt.
//
// # Why this is not a bearer-token endpoint
//
// This is the part that shaped everything else. An account with zero memberships
// cannot hold a token of any kind:
//
//   - [Service.Login] returns [ErrNoOrganization] before it ever reaches
//     startSession, so no access token and no refresh token are issued;
//   - [Issuer.Issue] refuses a principal whose TenantID is uuid.Nil, and
//     [Issuer.Verify] refuses a token whose org claim is the zero uuid;
//   - internal/api's principalFrom and PrincipalFromContext both reject a
//     principal with a zero tenant.
//
// Each of those is load-bearing and none of them should be relaxed: the tenant
// claim is the entire boundary between one organization's data and another's
// (ADR 0001, and internal/api/auth_middleware.go's header). So "authenticated but
// tenantless" is not a state the token model can currently represent, and making
// it representable would mean a second token type, a second verifier and a second
// middleware — three new things on the exact surface whose narrowness is the
// security property, to serve one endpoint that a given account calls at most
// once, ever.
//
// A password is the only credential a stranded account has. So this endpoint
// takes one, and authenticates it through [Service.verifyCredential] — the same
// function [Service.Login] uses, not a second implementation of it. The subject
// is whatever that verification returns and nothing else: there is no user id in
// the request struct, no email-to-act-as, no header. The organization is created
// for the account whose password was just verified, or it is not created.
//
// The cost of that choice is honest and worth stating: this is a fifth endpoint
// an anonymous caller can reach, and the fourth that accepts a password. It is
// budgeted by the same rate limiter as login (unlike registration — issue #73),
// carries the same tighter body limit as the other unauthenticated routes, and
// answers an unknown address and a wrong password identically, so it is not a
// new address-existence oracle.
//
// # What it deliberately does not do
//
// It does not create a *second* organization for an account that already has
// one, and still does not. That is [Service.CreateAdditionalOrganization] (issue
// #86), which is a different endpoint with a different credential: an account
// with a workspace has a token, and should not be made to re-present a password
// to use a feature. This one stays password-only because the account it serves
// structurally cannot hold a token — see above. An account with memberships
// still gets [ErrAlreadyHasOrganization] here.
//
// # The one-organization check is sequential, not serialized
//
// Stated precisely because the rest of this file states things absolutely. The
// membership read and the provisioning are two transactions — they have to be,
// for the same structural reason registration's two are — so two *concurrent*
// calls holding the correct password can both observe zero memberships and both
// provision. The loser is not detected, and the account ends up owning two
// workspaces, with [Service.Login] choosing between them by name.
//
// That is accepted rather than overlooked. It needs the account's own correct
// password and genuine concurrency, both organizations are correctly owned by
// that same account, and no boundary is crossed — it is a double-click producing
// a spare workspace. The obvious guard, a single-flight key in the Redis the
// rate limiter already uses, was considered and rejected: its failure mode is
// that a held or stuck key makes the repair path *unavailable*, and an account
// that cannot recover at all is a strictly worse outcome than an account with
// one workspace too many. This endpoint exists precisely to stop accounts being
// stuck.
//
// So the guarantee is "an account that already has an organization is refused",
// which is what the code does and what the tests assert, and not "an account can
// never come to have two", which nothing here enforces.
//
// It also does not issue a session. Token issuance stays in the three places the
// package doc names — Login, Refresh, SwitchOrganization — each of which derives
// the tenant from a membership it has just read. Adding a fourth writer of
// [Principal.TenantID] to save the client one round trip would trade the clearest
// invariant in this package for a convenience. The client logs in afterwards, and
// that login now succeeds.

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"context"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// ErrAlreadyHasOrganization means the subject authenticated but already belongs
// to at least one organization, so there is nothing to repair.
//
// Distinguished from the other refusals because it is not a failure of
// authentication or of authorization: the caller proved who they are and the
// answer is that this endpoint has nothing to do for them. It is reachable only
// *after* a correct password, so it discloses nothing to anyone who does not
// already hold the credential.
var ErrAlreadyHasOrganization = errors.New("auth: account already belongs to an organization")

// maxOwnedOrganizations is how many workspaces one account may own (issue #86).
//
// # The rule, and why it is a number rather than "as many as you like"
//
// Any authenticated account may create a workspace. There is no role check and
// no allowlist: the product implies "create a workspace" is a thing a person
// does, [Service.SwitchOrganization] and the web's org switcher already handle
// an account belonging to several, and a rule that only let existing owners
// create one would mean somebody invited into a colleague's workspace could
// never start their own.
//
// What there is instead is a ceiling, because an uncapped create-workspace
// endpoint is an uncapped **tenant factory**, and a tenant is the unit Stripe
// will bill (#160). Uncapped, one account can mint arbitrarily many free tenants
// and therefore arbitrarily many free seats, and the cap would then have to be
// introduced retroactively against accounts already over it.
//
// Five is deliberately low and deliberately reversible. Raising a cap is a
// one-line change nobody notices; lowering one strands accounts that are already
// over it and needs a grandfathering rule, so the cheap direction is the one to
// start in.
//
// # Why it counts what the account OWNS, not what it belongs to
//
// A membership someone else granted must not consume this budget. Being invited
// into five colleagues' workspaces is not a reason to be unable to create your
// own, and it would also hand every admin a way to exhaust another account's
// quota by adding them to things. Ownership is also what maps to billing: the
// owner is who pays for a tenant, so the owner is who should be limited in how
// many they can create.
const maxOwnedOrganizations = 5

// ErrTooManyOrganizations means the account is at [maxOwnedOrganizations].
//
// Not an authorization failure: the caller may create workspaces, and has
// created as many as they are allowed. That is why it maps to 409 rather than
// 403 — the same reading as [ErrAlreadyHasOrganization], which it sits beside.
var ErrTooManyOrganizations = errors.New("auth: account owns the maximum number of organizations")

// CreateOrganizationInput is a password, and the workspace to create with it.
//
// There is no user id and no subject field. The account acted on is the one the
// password verifies against — see the file header. ClientIP is used only for
// rate limiting, exactly as [LoginInput]'s is.
type CreateOrganizationInput struct {
	Email            string
	Password         string
	OrganizationName string
	ClientIP         string
}

// CreateOrganizationResult describes the organization that now exists and the
// account that owns it.
type CreateOrganizationResult struct {
	UserID           uuid.UUID
	OrganizationID   uuid.UUID
	OrganizationName string
	OrganizationSlug string
	Role             string
}

// CreateFirstOrganization gives an account with no organization one, after
// verifying its password.
//
// The order is the order it has to be:
//
//  1. count the attempt against both rate-limit budgets, before any work;
//  2. verify the credential, through the same function login uses, so an
//     unknown address costs the same as a wrong password and returns the same
//     error;
//  3. ask which organizations the *verified* subject belongs to, and refuse if
//     the answer is not "none";
//  4. run registration's second transaction for that subject.
//
// Step 4 is [Service.provisionOrganization], which is the same function
// [Service.Register] calls — not a copy of it. An organization created here is
// therefore identical to one created by a registration that did not fail,
// including the owner membership, which is the property that matters: a repair
// that produced a subtly different organization would be a permissions bug
// waiting to be found much later.
func (s *Service) CreateFirstOrganization(ctx context.Context, in CreateOrganizationInput) (CreateOrganizationResult, error) {
	email := NormalizeEmail(in.Email)

	if decision := s.limiter.Allow(ctx, email, in.ClientIP); !decision.Allowed {
		s.logger.WarnContext(ctx, "organization creation rate limited",
			slog.String("event", "auth.organization.rate_limited"),
			slog.String("scope", decision.Scope),
			slog.Duration("retry_after", decision.RetryAfter),
			slog.String("client_ip", in.ClientIP),
		)

		return CreateOrganizationResult{}, &RateLimitError{RetryAfter: decision.RetryAfter}
	}

	subject, err := s.verifyCredential(ctx, operationCreateOrganization, email, in.Password, in.ClientIP)
	if err != nil {
		return CreateOrganizationResult{}, err
	}

	// subject.ID, and there is no other expression in this function that could
	// appear here. It is what the password verified against.
	organizations, err := s.organizations(ctx, subject.ID)
	if err != nil {
		return CreateOrganizationResult{}, err
	}

	if len(organizations) > 0 {
		s.logger.InfoContext(ctx, "organization creation refused: the account already has one",
			slog.String("event", "auth.organization.already_exists"),
			slog.String("user_id", subject.ID.String()),
			slog.Int("organizations", len(organizations)),
		)

		return CreateOrganizationResult{}, ErrAlreadyHasOrganization
	}

	organization, err := s.provisionOrganization(ctx, subject.ID,
		workspaceName(in.OrganizationName, subject.DisplayName))
	if err != nil {
		s.logger.ErrorContext(ctx, "creating a first organization failed",
			slog.String("event", "auth.organization.create_failed"),
			slog.String("user_id", subject.ID.String()),
			slog.Any("error", err),
		)

		return CreateOrganizationResult{}, fmt.Errorf("creating the organization: %w", err)
	}

	// The counterpart to auth.register.partial: that event says an account was
	// left without an organization, this one says an account that had none now
	// has one. Both carry user_id, so the two can be matched into "how many
	// stranded accounts recovered themselves" by whoever reads the logs — today
	// by hand, since this service has no metrics or alerting yet (issue #12).
	s.logger.InfoContext(ctx, "first organization created for an account that had none",
		slog.String("event", "auth.organization.created"),
		slog.String("user_id", subject.ID.String()),
		slog.String("tenant_id", organization.ID.String()),
	)

	return CreateOrganizationResult{
		UserID:           subject.ID,
		OrganizationID:   organization.ID,
		OrganizationName: organization.Name,
		OrganizationSlug: organization.Slug,
		Role:             RoleOwner,
	}, nil
}

// CreateAdditionalOrganizationInput names the workspace to create. It does not
// name the account.
//
// The subject is a separate argument to [Service.CreateAdditionalOrganization]
// and comes from the verified token, so there is structurally nowhere in this
// struct to put somebody else's id — the same property [CreateOrganizationInput]
// has, arrived at the same way.
type CreateAdditionalOrganizationInput struct {
	Name string
}

// CreateAdditionalOrganization gives an already-authenticated account another
// workspace (issue #86).
//
// # Why this is a token endpoint and [Service.CreateFirstOrganization] is not
//
// They are the same operation for two accounts in different states, and the
// difference is which credential the account has. A subject with zero
// memberships cannot hold a token at all — four separate places in the auth path
// refuse a nil tenant, and each of them *is* the tenant boundary — so the repair
// path takes a password. An account that already has a workspace has a token,
// and making it re-present a password to use an ordinary feature would be
// friction with nothing behind it.
//
// So they are two routes rather than one route with two authentication modes.
// router_test.go asserts exactly which routes answer without a token, and that
// assertion is only meaningful while "is this route public?" has one answer per
// route; a route that is sometimes public would make the anonymous surface
// unanswerable from the route table, which is the property that test exists to
// protect.
//
// # The authorization rule
//
// Any authenticated account, up to [maxOwnedOrganizations] owned. See that
// constant for why there is a ceiling at all and why it counts ownership rather
// than membership.
//
// # What it does not do
//
// It does not issue a session, and it does not switch the caller into the new
// workspace. Token issuance stays in the three places the package doc names, each
// of which derives the tenant from a membership it has just read; the caller's
// next call is [Service.SwitchOrganization], which is the existing way to move
// between workspaces and already does the membership read that makes the new
// token honest. Returning a token here would make this a fourth writer of
// [Principal.TenantID] to save one round trip.
//
// # The cap check is sequential, not serialized
//
// The same caveat [Service.CreateFirstOrganization] states, for the same
// structural reason: the membership read and the provisioning are two
// transactions, so two concurrent calls can both observe four owned workspaces
// and both provision, leaving six. It needs the account's own valid token and
// genuine concurrency, every workspace is correctly owned by that account, and
// no boundary is crossed. A Redis single-flight was rejected there because a
// stuck key would make the repair path unavailable; here the trade is milder
// still, because the failure is one workspace over a soft ceiling rather than an
// account that cannot recover.
func (s *Service) CreateAdditionalOrganization(
	ctx context.Context,
	userID uuid.UUID,
	in CreateAdditionalOrganizationInput,
) (CreateOrganizationResult, error) {
	// Required rather than defaulted, which is the one place this deliberately
	// differs from the other two paths. "<display name>'s workspace" exists
	// because registration has to produce *something* for a person who was not
	// asked; somebody deliberately creating a second workspace has been asked,
	// and a second "Andy's workspace" beside the first is not a kindness. It
	// also saves a profile read this function otherwise has no reason to make.
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return CreateOrganizationResult{}, fmt.Errorf("%w: workspace name is required", ErrInvalidInput)
	}

	organizations, err := s.organizations(ctx, userID)
	if err != nil {
		return CreateOrganizationResult{}, err
	}

	owned := 0

	for _, organization := range organizations {
		if organization.Role == RoleOwner {
			owned++
		}
	}

	if owned >= maxOwnedOrganizations {
		s.logger.InfoContext(ctx, "organization creation refused: the account is at the cap",
			slog.String("event", "auth.organization.cap_reached"),
			slog.String("user_id", userID.String()),
			slog.Int("owned", owned),
			slog.Int("cap", maxOwnedOrganizations),
		)

		return CreateOrganizationResult{}, ErrTooManyOrganizations
	}

	// userID, and there is no other expression in this function that could
	// appear here. It is the subject the token verified as; nothing from the
	// request body reaches this call.
	organization, err := s.provisionOrganization(ctx, userID, name)
	if err != nil {
		s.logger.ErrorContext(ctx, "creating an additional organization failed",
			slog.String("event", "auth.organization.create_failed"),
			slog.String("user_id", userID.String()),
			slog.Any("error", err),
		)

		return CreateOrganizationResult{}, fmt.Errorf("creating the organization: %w", err)
	}

	s.logger.InfoContext(ctx, "additional organization created",
		slog.String("event", "auth.organization.created"),
		slog.String("user_id", userID.String()),
		slog.String("tenant_id", organization.ID.String()),
		slog.Int("owned_before", owned),
	)

	return CreateOrganizationResult{
		UserID:           userID,
		OrganizationID:   organization.ID,
		OrganizationName: organization.Name,
		OrganizationSlug: organization.Slug,
		Role:             RoleOwner,
	}, nil
}

// RenameOrganizationInput is the new name, and nothing that names an
// organization.
//
// The tenant is the caller's verified `org` claim, which is where every other
// tenant-scoped route gets it. There is no organization id in this struct, in
// the route, or in [Service.RenameOrganization]'s signature — the same absence
// [AddMemberInput] has, and the reason auth_bola_test.go's attack on this route
// is short.
type RenameOrganizationInput struct {
	Principal Principal
	Name      string
}

// RenameOrganization changes the current workspace's name (issue #90).
//
// # Why the name was permanent until now
//
// `POST /organizations` was the only route under that path and nothing anywhere
// else wrote `organizations.name`, so the name an organization was created with
// was the name it had forever. That was easy to miss while the only way to make
// one was registration, where the user types it and can see what they are
// choosing. #85 made it visible: the recovery screen creates a workspace for an
// account whose first attempt was interrupted, and the name that account chose
// the first time went with the transaction that never committed — so the form
// had to ask again, under a hint reading "It cannot be renamed later."
//
// # Authorization: owner or admin, read inside the transaction
//
// The same rule `POST /members` applies, and read the same way — the caller's
// membership comes from a tenant-scoped query rather than from the token's role
// claim. ADR 0008 explains why that matters: a role claim is minted at login and
// re-derived at most once per access-token lifetime, so a demoted account
// carries a stale one, and a caller whose membership was revoked entirely finds
// no row and is refused as [ErrNotAMember] rather than acting on a workspace
// they have been removed from.
//
// Whether renaming the workspace should be *stricter* than adding a member —
// owner only — is a real question and the answer here is no, deliberately: #90
// asked for parity with `POST /members`, and inventing a third authorization
// tier for one endpoint is the kind of thing that is easy to add and impossible
// to remember.
//
// # The slug is frozen, and that is a decision
//
// It is not recomputed on rename. Two reasons, in this order:
//
//   - `organizations_slug_key` is a global unique index, so regenerating would
//     make a rename fail whenever another tenant already holds the slug for that
//     name — turning a cosmetic edit into a 409 about somebody else's workspace,
//     which is also a small existence oracle.
//   - it appears in **no URL** in this application. Not in `apps/web`'s routes,
//     not in any API path. So there is nothing for a fresh slug to fix.
//
// The moment a slug does appear in a URL, "regenerate and redirect" becomes a
// real design with real work in it — and it should be decided then, against a
// URL that exists, rather than pre-emptively now against one that does not.
func (s *Service) RenameOrganization(ctx context.Context, in RenameOrganizationInput) (Organization, error) {
	name := strings.TrimSpace(in.Name)

	if name == "" {
		return Organization{}, fmt.Errorf("%w: workspace name is required", ErrInvalidInput)
	}

	// The bound from #67, applied here so a rename cannot reintroduce the
	// unbounded field by the back door. provisionOrganization's own guard does
	// not cover this path -- nothing here goes through it -- which is exactly
	// the third writer that migration 00007's CHECK was added for.
	if err := validateWorkspaceName(name); err != nil {
		return Organization{}, err
	}

	var renamed store.Organization

	err := s.store.WithTenant(ctx, in.Principal.TenantID, func(ctx context.Context, q store.Querier) error {
		if aerr := authorizeRename(ctx, q, in.Principal.UserID); aerr != nil {
			return aerr
		}

		// Same transaction as the authorization read, so the role the decision
		// was made against is the role the write is checked against. The
		// alternative -- authorize, then write -- is a window in which a
		// membership can be revoked between the two.
		organization, uerr := q.RenameOrganization(ctx, name)
		if uerr != nil {
			return uerr
		}

		renamed = organization

		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotAMember) || errors.Is(err, ErrInsufficientRole) {
			s.logger.InfoContext(ctx, "workspace rename refused",
				slog.String("event", "auth.organization.rename_refused"),
				slog.String("actor_user_id", in.Principal.UserID.String()),
				slog.String("tenant_id", in.Principal.TenantID.String()),
				// The reason, not the name: a refused rename says nothing about
				// what the caller tried to call it.
				slog.String("reason", refusalReason(err)),
			)

			return Organization{}, err
		}

		return Organization{}, fmt.Errorf("renaming the organization: %w", err)
	}

	s.logger.InfoContext(ctx, "workspace renamed",
		slog.String("event", "auth.organization.renamed"),
		slog.String("actor_user_id", in.Principal.UserID.String()),
		slog.String("tenant_id", renamed.ID.String()),
	)

	return Organization{
		ID:   renamed.ID,
		Name: renamed.Name,
		Slug: renamed.Slug,
		Role: in.Principal.Role,
	}, nil
}

// authorizeRename decides whether one member of the current tenant may rename
// it.
//
// Deliberately a sibling of [authorizeAddMember] rather than a call into it:
// that function's signature takes the role being *granted*, which this operation
// does not have, and giving it a sentinel would make one function answer two
// unrelated questions. What they share is the part that matters — the membership
// is read from the current tenant's rows, so RLS supplies
// `tenant_id = current_tenant_id()` and a token for an organization the caller
// has been removed from finds no row.
func authorizeRename(ctx context.Context, q store.Querier, actorID uuid.UUID) error {
	membership, err := q.GetMembership(ctx, actorID)
	if errors.Is(err, store.ErrNoRows) {
		return ErrNotAMember
	} else if err != nil {
		return fmt.Errorf("reading the caller's membership: %w", err)
	}

	switch membership.Role {
	case RoleOwner, RoleAdmin:
		return nil
	default:
		return ErrInsufficientRole
	}
}

// refusalReason names why a rename was refused, for the log line only.
func refusalReason(err error) string {
	if errors.Is(err, ErrNotAMember) {
		return "not_a_member"
	}

	return "insufficient_role"
}
