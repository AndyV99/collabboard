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
// one. That is a real feature and issue #34 names it as the reason to prefer
// this endpoint's shape over a self-healing login — but it is a different
// authorization question (who may create a workspace, and how many) and it is not
// what the issue asked for. An account with memberships gets
// [ErrAlreadyHasOrganization]. Lifting that is one condition plus an answer to
// the authorization question; filed as issue #86 rather than guessed at here.
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

	"context"

	"github.com/google/uuid"
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
