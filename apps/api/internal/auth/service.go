// Package auth is the identity half of the service: registration, login,
// sessions, and the principal an authenticated request acts as.
//
// # The one thing to get right
//
// [Principal.TenantID] is the value internal/store puts into app.tenant_id, and
// ADR 0001 is explicit that store.WithTenant does not check it: row-level
// security faithfully serves whatever tenant it is handed. Isolation is
// isolation, not authorization. So the boundary between "a user of organization
// A" and "organization B's data" is drawn exactly here, by this package
// deciding what goes in that field.
//
// It is populated in precisely three places, all of them in this file:
//
//   - [Service.Login], from the memberships the pre-tenant path returns for the
//     authenticated subject;
//   - [Service.Refresh], from the same lookup, re-run — so a membership revoked
//     mid-session stops working at the next refresh rather than at expiry;
//   - [Service.SwitchOrganization], from the same lookup, filtered to the
//     requested organization, which is the only place in the service that takes
//     an organization id from a client and the only place that has to reject
//     one.
//
// Nothing reads a tenant from a header, a path segment, a query parameter or a
// request body. The HTTP layer has no route or field that could carry one. That
// is asserted in internal/api/auth_bola_test.go rather than merely arranged.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// The errors a caller has to distinguish. Everything else is wrapped and
// reported as a server failure.
var (
	// ErrInvalidCredentials is the answer to every failed login, whatever the
	// reason: no such address, no password set, wrong password. One error
	// because one response — see [Service.Login].
	ErrInvalidCredentials = errors.New("auth: invalid credentials")

	// ErrEmailTaken means registration hit the unique index on users.
	ErrEmailTaken = errors.New("auth: email is already registered")

	// ErrNoOrganization means the subject authenticated but belongs to no
	// organization, so there is no tenant to issue a token for. Distinguished
	// from ErrInvalidCredentials because it is *not* an authentication failure
	// and telling the user "your account works but has no workspace" is the
	// only actionable thing to say.
	ErrNoOrganization = errors.New("auth: account belongs to no organization")

	// ErrNotAMember means a request named an organization the subject does not
	// belong to. This is the BOLA case, and it is an authorization failure
	// rather than a not-found: the caller is authenticated, they are simply not
	// entitled.
	ErrNotAMember = errors.New("auth: not a member of that organization")

	// ErrInvalidInput means the request did not describe a registerable
	// account. Wrapped with a specific reason, which is safe to show: it is
	// about the submitted values, not about anything stored.
	ErrInvalidInput = errors.New("auth: invalid input")
)

// Password length bounds.
//
// The floor is 12 rather than 8. NIST SP 800-63B's minimum is 8 for
// machine-generated secrets; for user-chosen ones the same document leans on
// length over composition rules, and 12 is where the common wordlists stop
// being cheap. There are no composition rules here on purpose — they push
// people towards "Password1!" and towards reuse.
//
// The ceiling exists because argon2id costs memory proportional to nothing the
// user controls *except* how long the input is to hash; unbounded input is a
// free way to make the server do more work than the attacker. 128 is far above
// any real passphrase.
const (
	MinPasswordLength = 12
	MaxPasswordLength = 128

	// maxEmailLength is the practical limit; RFC 5321 puts the path at 254
	// octets.
	maxEmailLength = 254

	// maxDisplayNameLength keeps a display name from being a payload.
	maxDisplayNameLength = 128

	// maxOrganizationNameLength bounds the workspace name, in runes.
	//
	// 200 rather than 128, because this is the same *kind* of value as a
	// project, board, column or card title and those are `maxNameLength` in
	// internal/api/crud.go. The number is repeated rather than imported:
	// internal/auth cannot import internal/api (the dependency runs the other
	// way), and a shared constants package for one integer would be worse than
	// the duplication it removed. The test below pins them together.
	//
	// It was the one user-supplied field on this API with no bound at all --
	// #50's 16 KiB body limit made the practical ceiling "whatever fits", which
	// is containment rather than validation, and an 8,000-character name was
	// still accepted, stored, slugged and rendered into every member's UI.
	maxOrganizationNameLength = 200
)

// Store is the slice of internal/store this package uses.
//
// An interface, so the service's logic — especially the "do the derivation
// even when the account does not exist" property — is testable without a
// container. The real implementation is *store.Store; internal/store is still
// the only package that talks to Postgres.
type Store interface {
	WithoutTenant(ctx context.Context, reason store.IdentityReason, fn store.IdentityFunc) error
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error
}

// Service is the auth use cases. It owns no HTTP concepts; internal/api adapts
// it.
type Service struct {
	store    Store
	deriver  Deriver
	issuer   *Issuer
	sessions *SessionStore
	limiter  *Limiter
	logger   *slog.Logger

	params Argon2Params

	// absentSalt is the salt used to derive against when no account exists.
	// Derived once from the signing secret, so it is stable across the process
	// and unguessable from outside. See loginSubject.
	absentSalt []byte
}

// ServiceDeps are the collaborators [NewService] needs.
type ServiceDeps struct {
	Store    Store
	Deriver  Deriver
	Issuer   *Issuer
	Sessions *SessionStore
	Limiter  *Limiter
	Logger   *slog.Logger
	Params   Argon2Params

	// AbsentSalt is used for the derivation performed when no account matches.
	// It must not be empty; cmd/api derives it from the signing secret.
	AbsentSalt []byte
}

// NewService wires the service, rejecting a configuration that would leave one
// of its guarantees unmet.
func NewService(deps ServiceDeps) (*Service, error) {
	switch {
	case deps.Store == nil:
		return nil, errors.New("auth: store is required")
	case deps.Deriver == nil:
		return nil, errors.New("auth: deriver is required")
	case deps.Issuer == nil:
		return nil, errors.New("auth: token issuer is required")
	case deps.Sessions == nil:
		return nil, errors.New("auth: session store is required")
	case deps.Limiter == nil:
		return nil, errors.New("auth: rate limiter is required")
	case deps.Logger == nil:
		return nil, errors.New("auth: logger is required")
	case len(deps.AbsentSalt) < DefaultArgon2SaltLength:
		// Not cosmetic. If this were empty, the derivation done for an unknown
		// address would use a different-shaped input from a real one, and the
		// timing difference is the enumeration oracle this whole design is
		// avoiding.
		return nil, fmt.Errorf("auth: absent-account salt is %d bytes, minimum %d",
			len(deps.AbsentSalt), DefaultArgon2SaltLength)
	}

	if err := deps.Params.Validate(); err != nil {
		return nil, err
	}

	return &Service{
		store:      deps.Store,
		deriver:    deps.Deriver,
		issuer:     deps.Issuer,
		sessions:   deps.Sessions,
		limiter:    deps.Limiter,
		logger:     deps.Logger,
		params:     deps.Params,
		absentSalt: deps.AbsentSalt,
	}, nil
}

// TokenPair is what a successful login, refresh or organization switch returns.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	ExpiresIn    time.Duration
}

// RegisterInput is a new account and the workspace it starts with.
type RegisterInput struct {
	Email            string
	Password         string
	DisplayName      string
	OrganizationName string
}

// RegisterResult describes what registration created.
type RegisterResult struct {
	UserID           uuid.UUID
	Email            string
	DisplayName      string
	OrganizationID   uuid.UUID
	OrganizationName string
	OrganizationSlug string
}

// Register creates a global identity, its credential, and a first organization
// the identity owns.
//
// # Two transactions, and why that is not atomic
//
// The user and its credential are created together in one pre-tenant
// transaction — a user row can never be committed without the password that
// makes it usable. The organization and the membership are created in a
// *second*, tenant-scoped transaction, because the tenant does not exist until
// the first statement of it runs and the two doors are separate transactions by
// construction (see internal/store).
//
// So a failure between them leaves an account with no organization. That is
// visible rather than silent: login reports ErrNoOrganization, which the HTTP
// layer turns into a 403 that says so. It is not compensated by deleting the
// user because the pre-tenant path deliberately has no delete — ADR 0002 —
// and adding one to paper over this would be a much worse trade than the
// failure mode it fixes.
//
// # How the account gets out of that state (issue #34)
//
// It calls [Service.CreateFirstOrganization], which is the same second
// transaction — literally [Service.provisionOrganization], the function this one
// calls — run again later against an account that already exists. The window is
// still there and still logged as auth.register.partial; what changed is that
// the account can now leave it without an operator. See organizations.go.
func (s *Service) Register(ctx context.Context, in RegisterInput) (RegisterResult, error) {
	email := NormalizeEmail(in.Email)
	displayName := strings.TrimSpace(in.DisplayName)

	// Resolved here rather than at the call to provisionOrganization below, so
	// that the name -- the requested one, or the default when none was given --
	// is validated *before* the account exists.
	//
	// That ordering is the whole reason this is not left to the funnel guard in
	// provisionOrganization. Registration is two transactions, and a failure
	// between them strands an account that can authenticate and belongs
	// nowhere (see the comment above this function, and #34). An over-long
	// workspace name is a 400 the caller can fix; it must not also be a way to
	// manufacture that state on demand.
	organizationName := workspaceName(in.OrganizationName, displayName)

	if err := validateRegistration(email, in.Password, displayName, organizationName); err != nil {
		return RegisterResult{}, err
	}

	salt, err := NewSalt(s.params)
	if err != nil {
		return RegisterResult{}, err
	}

	key, err := s.derive(ctx, in.Password, salt, s.params)
	if err != nil {
		return RegisterResult{}, err
	}

	var user store.CreatedUser

	err = s.store.WithoutTenant(ctx, store.ReasonRegisterUser, func(ctx context.Context, q store.IdentityQuerier) error {
		created, cerr := q.CreateUser(ctx, store.CreateUserParams{Email: email, DisplayName: displayName})
		if cerr != nil {
			return cerr
		}

		if _, cerr = q.CreatePassword(ctx, store.CreatePasswordParams{
			UserID:      created.ID,
			Salt:        salt,
			MemoryKib:   int32(s.params.MemoryKiB),  //nolint:gosec // bounded by Argon2Params.Validate and the CHECK in migration 00005
			Iterations:  int32(s.params.Iterations), //nolint:gosec // bounded by Argon2Params.Validate and the CHECK in migration 00005
			Parallelism: int32(s.params.Parallelism),
			KeyLength:   int32(s.params.KeyLength), //nolint:gosec // bounded by Argon2Params.Validate and the CHECK in migration 00005
			Key:         key,
		}); cerr != nil {
			return cerr
		}

		user = created

		return nil
	})
	if err != nil {
		if store.IsUniqueViolation(err) {
			s.logger.InfoContext(ctx, "registration rejected: address already registered",
				slog.String("event", "auth.register.rejected"),
				slog.String("reason", "email_taken"),
			)

			return RegisterResult{}, ErrEmailTaken
		}

		return RegisterResult{}, fmt.Errorf("creating the account: %w", err)
	}

	organization, err := s.provisionOrganization(ctx, user.ID, organizationName)
	if err != nil {
		s.logger.ErrorContext(ctx, "registration created an account with no organization",
			slog.String("event", "auth.register.partial"),
			slog.String("user_id", user.ID.String()),
			slog.Any("error", err),
		)

		return RegisterResult{}, fmt.Errorf("creating the organization: %w", err)
	}

	s.logger.InfoContext(ctx, "account registered",
		slog.String("event", "auth.register.success"),
		slog.String("user_id", user.ID.String()),
		slog.String("tenant_id", organization.ID.String()),
	)

	return RegisterResult{
		UserID:           user.ID,
		Email:            user.Email,
		DisplayName:      user.DisplayName,
		OrganizationID:   organization.ID,
		OrganizationName: organization.Name,
		OrganizationSlug: organization.Slug,
	}, nil
}

// RoleOwner is the membership role registration grants the account that created
// the organization.
const RoleOwner = "owner"

// The endpoints that accept a password, as they appear in a failed-attempt log
// line. A closed set of literals rather than a caller-supplied string, so the
// field cannot carry attacker-controlled text into the logs.
const (
	operationLogin              = "login"
	operationCreateOrganization = "create_organization"
)

// provisionOrganization creates an organization and the owner membership that
// makes userID its first member, in one tenant-scoped transaction.
//
// This is registration's *second* transaction, and it is a function rather than
// a block inside [Service.Register] because [Service.CreateFirstOrganization]
// runs the same one to repair a registration that failed between the two (issue
// #34). Two code paths that each created an organization plus an owner
// membership would drift, and the drift would be a permissions bug — an
// organization whose creator is not its owner, or is not a member of it at all.
// So there is one.
//
// The tenant id is generated here and set as the transaction's tenant, then used
// as the organization's primary key by the INSERT itself. An organization *is*
// its tenant; there is no separate identifier to keep in sync, and no argument
// the caller could get wrong.
//
// userID is the only thing a caller supplies about *who* this belongs to, and
// both callers pass a subject they have already authenticated: Register passes
// the row it just created, CreateFirstOrganization passes the id a password
// verification returned. Neither passes anything that arrived from a request.
func (s *Service) provisionOrganization(ctx context.Context, userID uuid.UUID, name string) (store.Organization, error) {
	tenantID := uuid.New()

	var organization store.Organization

	// Every write to organizations.name goes through here, so this is where the
	// bound cannot be forgotten. Register checks the same rule earlier for a
	// different reason -- see the comment there -- and this one is what covers
	// CreateFirstOrganization and whatever calls it next.
	if err := validateWorkspaceName(name); err != nil {
		return store.Organization{}, err
	}

	err := s.store.WithTenant(ctx, tenantID, func(ctx context.Context, q store.Querier) error {
		created, cerr := q.CreateOrganization(ctx, store.CreateOrganizationParams{
			Name: name,
			Slug: newSlug(name),
		})
		if cerr != nil {
			return cerr
		}

		// No tenant argument and no organization argument: the membership lands
		// in the transaction's tenant, which is the organization created one
		// statement ago. See internal/store's query conventions.
		if _, cerr = q.CreateMembership(ctx, store.CreateMembershipParams{
			UserID: userID,
			Role:   RoleOwner,
		}); cerr != nil {
			return cerr
		}

		organization = created

		return nil
	})
	if err != nil {
		return store.Organization{}, err
	}

	return organization, nil
}

// workspaceName is the name an organization ends up with: what the caller
// asked for, or a default built from their display name.
//
// Shared by both callers of [Service.provisionOrganization] so that a workspace
// created by the repair path is named the way one created by registration would
// have been.
//
// Note what is *not* here: a length bound. There is none on registration either
// — organization_name is the one unbounded user-supplied field on this service,
// which is issue #67, and matching that gap deliberately is better than fixing
// it in half the places. When #67 is fixed the bound belongs in this function,
// where it covers both callers and the generated default at once.
func workspaceName(requested, displayName string) string {
	if name := strings.TrimSpace(requested); name != "" {
		return name
	}

	return displayName + "'s workspace"
}

// validateWorkspaceName bounds the one name a caller can set without a
// credential.
//
// Blank is not checked here and is not an oversight: [workspaceName] has
// already substituted the default by the time anything is validated, and the
// column's own `CHECK (length(btrim(name)) > 0)` is the backstop for a default
// that somehow came out empty.
//
// The generated default is subject to the same cap. It cannot exceed it today —
// a 128-rune display name plus "'s workspace" is 140 — but "cannot today" is a
// fact about `maxDisplayNameLength`, and the two numbers have no reason to know
// about each other.
func validateWorkspaceName(name string) error {
	if utf8.RuneCountInString(name) > maxOrganizationNameLength {
		return fmt.Errorf("%w: workspace name must be at most %d characters",
			ErrInvalidInput, maxOrganizationNameLength)
	}

	return nil
}

// LoginInput is a credential presentation. ClientIP is used only for rate
// limiting.
type LoginInput struct {
	Email    string
	Password string
	ClientIP string
}

// LoginResult is a principal and the tokens that represent it.
type LoginResult struct {
	Principal    Principal
	Tokens       TokenPair
	Organization Organization
}

// Organization is one membership, as the auth layer sees it.
type Organization struct {
	ID   uuid.UUID
	Name string
	Slug string
	Role string
}

// Login verifies a password and issues a session.
//
// # Not revealing whether an address exists
//
// Every path through this function does the same work in the same order:
//
//  1. count the attempt against both rate-limit budgets;
//  2. look the address up;
//  3. read KDF parameters — the account's own, or a stand-in derived from the
//     service's secret when there is no account or no password;
//  4. run exactly one argon2id derivation, which is ~99% of the wall time;
//  5. ask the database to compare, against the real user id or a random one.
//
// Steps 3 to 5 are not skipped when the account is absent. That is the whole
// mechanism: the expensive step happens either way, so an unknown address and a
// wrong password cost the same, and both return [ErrInvalidCredentials], which
// the HTTP layer renders as one 401 with one body.
//
// The stand-in salt is derived from the signing secret rather than generated
// per attempt, because a per-attempt random salt is fine for timing but makes
// the *database* work differ: a real account's parameters come from a row, and
// consistency here keeps the two paths shaped alike.
//
// TestLoginDoesTheSameWorkForAnUnknownAddress asserts the derivation count
// directly, which is a stronger claim than a wall-clock comparison and does not
// flake on a loaded CI runner. A timing comparison exists too, with a loose
// bound, because the counting test would still pass if some other step became
// wildly asymmetric.
func (s *Service) Login(ctx context.Context, in LoginInput) (LoginResult, error) {
	email := NormalizeEmail(in.Email)

	if decision := s.limiter.Allow(ctx, email, in.ClientIP); !decision.Allowed {
		s.logger.WarnContext(ctx, "login rate limited",
			slog.String("event", "auth.login.rate_limited"),
			slog.String("scope", decision.Scope),
			slog.Duration("retry_after", decision.RetryAfter),
			slog.String("client_ip", in.ClientIP),
		)

		return LoginResult{}, &RateLimitError{RetryAfter: decision.RetryAfter}
	}

	subject, err := s.verifyCredential(ctx, operationLogin, email, in.Password, in.ClientIP)
	if err != nil {
		return LoginResult{}, err
	}

	verified := subject.ID

	organizations, err := s.organizations(ctx, verified)
	if err != nil {
		return LoginResult{}, err
	}

	if len(organizations) == 0 {
		s.logger.InfoContext(ctx, "login succeeded for an account with no organization",
			slog.String("event", "auth.login.no_organization"),
			slog.String("user_id", verified.String()),
		)

		return LoginResult{}, ErrNoOrganization
	}

	// The first organization by name, which is the order the pre-tenant query
	// returns. Deterministic, and a client that wants a different one calls
	// SwitchOrganization — where the membership is checked again.
	active := organizations[0]

	result, err := s.startSession(ctx, verified, active, uuid.Nil)
	if err != nil {
		return LoginResult{}, err
	}

	s.logger.InfoContext(ctx, "login succeeded",
		slog.String("event", "auth.login.success"),
		slog.String("user_id", verified.String()),
		slog.String("tenant_id", active.ID.String()),
		slog.String("session_id", result.Principal.SessionID.String()),
		slog.Int("organizations", len(organizations)),
	)

	return result, nil
}

// Refresh exchanges a refresh token for a new access token and a new refresh
// token, re-checking the membership on the way.
//
// The membership check is the reason refresh is more than a rotation. Nothing
// consults the database on a normal request — that is what makes a signed
// access token cheap — so removing someone from an organization would otherwise
// take effect only when their token expired. Checking here bounds the window to
// one access-token lifetime and costs one query at refresh frequency.
func (s *Service) Refresh(ctx context.Context, refreshToken string) (LoginResult, error) {
	session, rotated, err := s.sessions.Rotate(ctx, refreshToken)

	switch {
	case errors.Is(err, ErrRefreshReused):
		s.logger.WarnContext(ctx, "refresh token reuse detected; session revoked",
			slog.String("event", "auth.refresh.reuse_detected"),
		)

		return LoginResult{}, ErrRefreshReused
	case errors.Is(err, ErrRefreshUnknown):
		s.logger.InfoContext(ctx, "refresh rejected",
			slog.String("event", "auth.refresh.rejected"),
			slog.String("reason", "unknown_token"),
		)

		return LoginResult{}, ErrRefreshUnknown
	case err != nil:
		return LoginResult{}, fmt.Errorf("rotating the refresh token: %w", err)
	}

	organizations, err := s.organizations(ctx, session.UserID)
	if err != nil {
		return LoginResult{}, err
	}

	active, ok := findOrganization(organizations, session.TenantID)
	if !ok {
		// The membership that justified this session is gone. Revoke rather
		// than downgrade: the client asked to continue as a member of an
		// organization it is no longer in, and silently moving it to a
		// different tenant would be worse than making it log in.
		if rerr := s.sessions.RevokeSession(ctx, session.ID); rerr != nil {
			return LoginResult{}, fmt.Errorf("revoking a session whose membership is gone: %w", rerr)
		}

		s.logger.WarnContext(ctx, "refresh rejected: membership no longer exists",
			slog.String("event", "auth.refresh.membership_revoked"),
			slog.String("user_id", session.UserID.String()),
			slog.String("tenant_id", session.TenantID.String()),
		)

		return LoginResult{}, ErrNotAMember
	}

	principal := Principal{
		UserID:    session.UserID,
		TenantID:  active.ID,
		Role:      active.Role,
		SessionID: session.ID,
	}

	access, expires, err := s.issuer.Issue(principal)
	if err != nil {
		return LoginResult{}, err
	}

	principal.ExpiresAt = expires

	s.logger.InfoContext(ctx, "access token refreshed",
		slog.String("event", "auth.refresh.success"),
		slog.String("user_id", session.UserID.String()),
		slog.String("tenant_id", active.ID.String()),
		slog.String("session_id", session.ID.String()),
	)

	return LoginResult{
		Principal: principal,
		Tokens: TokenPair{
			AccessToken:  access,
			RefreshToken: rotated,
			ExpiresAt:    expires,
			ExpiresIn:    s.issuer.AccessTTL(),
		},
		Organization: active,
	}, nil
}

// Logout revokes the session a refresh token belongs to.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if err := s.sessions.Revoke(ctx, refreshToken); err != nil {
		return fmt.Errorf("revoking the session: %w", err)
	}

	s.logger.InfoContext(ctx, "session revoked",
		slog.String("event", "auth.logout"),
	)

	return nil
}

// SwitchOrganization issues a session for a different organization the subject
// belongs to.
//
// This is the only place in the service where an organization id arrives from a
// client, so it is the only place that can get object-level authorization
// wrong. The check is not "does this organization exist" — that would be a
// membership-disclosure endpoint — it is "is this organization in the list of
// memberships for the *authenticated subject*", and the list comes from the
// subject's id in the verified token, never from the request.
//
// A non-member gets [ErrNotAMember] and no token. Note what does *not* happen:
// the requested id is never used to set a tenant context, so even a bug in the
// response path could not leak rows.
func (s *Service) SwitchOrganization(ctx context.Context, principal Principal, target uuid.UUID) (LoginResult, error) {
	organizations, err := s.organizations(ctx, principal.UserID)
	if err != nil {
		return LoginResult{}, err
	}

	active, ok := findOrganization(organizations, target)
	if !ok {
		s.logger.WarnContext(ctx, "organization switch refused",
			slog.String("event", "auth.switch_organization.denied"),
			slog.String("user_id", principal.UserID.String()),
			slog.String("from_tenant_id", principal.TenantID.String()),
			slog.String("requested_tenant_id", target.String()),
		)

		return LoginResult{}, ErrNotAMember
	}

	// A fresh session rather than a re-issued token on the old one: the old
	// session's refresh token still names the old tenant, and leaving it live
	// would mean one login held two tenants at once with no way to tell them
	// apart in the logs.
	result, err := s.startSession(ctx, principal.UserID, active, principal.SessionID)
	if err != nil {
		return LoginResult{}, err
	}

	s.logger.InfoContext(ctx, "organization switched",
		slog.String("event", "auth.switch_organization.success"),
		slog.String("user_id", principal.UserID.String()),
		slog.String("from_tenant_id", principal.TenantID.String()),
		slog.String("tenant_id", active.ID.String()),
		slog.String("session_id", result.Principal.SessionID.String()),
	)

	return result, nil
}

// Organizations lists the organizations a subject belongs to. The subject is
// the authenticated principal's id and nothing else — the underlying function
// does not authorize, so passing an id from a request body would turn it into a
// membership-disclosure endpoint (ADR 0002 says so explicitly).
func (s *Service) Organizations(ctx context.Context, principal Principal) ([]Organization, error) {
	return s.organizations(ctx, principal.UserID)
}

// startSession issues a refresh token and an access token for one membership,
// optionally revoking the session it replaces.
func (s *Service) startSession(ctx context.Context, userID uuid.UUID, org Organization, replacing uuid.UUID) (LoginResult, error) {
	sessionID := uuid.New()

	refresh, err := s.sessions.Issue(ctx, Session{
		ID:       sessionID,
		UserID:   userID,
		TenantID: org.ID,
		Role:     org.Role,
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("issuing a refresh token: %w", err)
	}

	principal := Principal{
		UserID:    userID,
		TenantID:  org.ID,
		Role:      org.Role,
		SessionID: sessionID,
	}

	access, expires, err := s.issuer.Issue(principal)
	if err != nil {
		return LoginResult{}, err
	}

	principal.ExpiresAt = expires

	if replacing != uuid.Nil {
		if rerr := s.sessions.RevokeSession(ctx, replacing); rerr != nil {
			// Logged, not fatal: the new session is already live, and failing
			// the request now would leave the caller with a token they were
			// never told about.
			s.logger.ErrorContext(ctx, "could not revoke the replaced session",
				slog.String("event", "auth.session.revoke_failed"),
				slog.String("session_id", replacing.String()),
				slog.Any("error", rerr),
			)
		}
	}

	return LoginResult{
		Principal: principal,
		Tokens: TokenPair{
			AccessToken:  access,
			RefreshToken: refresh,
			ExpiresAt:    expires,
			ExpiresIn:    s.issuer.AccessTTL(),
		},
		Organization: org,
	}, nil
}

// verifyCredential resolves an address and password to the account they belong
// to, or to [ErrInvalidCredentials].
//
// It is steps 2 to 5 of [Service.Login]'s list, extracted verbatim, and it is
// extracted because [Service.CreateFirstOrganization] has to perform exactly the
// same check — an account with no organization has no token, so a password is
// the only credential it can present (issue #34, and see organizations.go).
// Sharing the function rather than the description is what keeps the
// anti-enumeration property true at both endpoints: one argon2id derivation
// whatever is wrong, the same two pre-tenant lookups in the same order, and the
// same error for an unknown address as for a wrong password.
//
// It does not count the attempt against the rate-limit budgets. That is step 1,
// and it stays with the callers, because the budget is per *endpoint attempt*
// and a caller that forgot it should be a missing line in a short function
// rather than a silently absent behaviour inside a long one.
//
// operation is the label a failed attempt is logged under, so that the two
// endpoints presenting a password remain distinguishable to an operator. It
// changes nothing a caller can observe — see [Service.logFailedLogin].
func (s *Service) verifyCredential(
	ctx context.Context,
	operation, email, password, clientIP string,
) (store.IdentityUser, error) {
	userID, user, candidate := s.loginSubject(ctx, email)

	key, err := s.derive(ctx, password, candidate.salt, candidate.params)
	if err != nil {
		return store.IdentityUser{}, err
	}

	verified, err := withoutTenant(ctx, s.store, store.ReasonVerifyPassword,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.VerifyPassword(ctx, store.VerifyPasswordParams{UserID: userID, Key: key})
		})

	switch {
	case errors.Is(err, store.ErrNotFound):
		s.logFailedLogin(ctx, operation, "credentials", clientIP)

		return store.IdentityUser{}, ErrInvalidCredentials
	case err != nil:
		return store.IdentityUser{}, fmt.Errorf("verifying the password: %w", err)
	case verified != userID:
		// Cannot happen through the SQL — the function filters on the id it was
		// given — but a mismatch here would mean the wrong account was about to
		// be issued a token, which is worth refusing rather than trusting.
		return store.IdentityUser{}, ErrInvalidCredentials
	case user.ID != verified:
		// Equally unreachable, and refused for the equally blunt reason: the
		// row returned here is what the caller goes on to act *for*. A password
		// that verified against one account must not hand back another one's
		// identity. Same check, and same reasoning, as Service.profile's.
		return store.IdentityUser{}, ErrInvalidCredentials
	}

	return user, nil
}

// loginCandidate is the subject a login attempt will be checked against, real
// or invented.
type loginCandidate struct {
	salt   []byte
	params Argon2Params
}

// loginSubject resolves an address to the user id and KDF parameters a login
// will use, inventing both when the account does not exist or has no password.
//
// Errors are swallowed on purpose and folded into the absent case. A database
// failure here must not produce a different-shaped response from a missing
// account, or the difference *is* the oracle. The failure still surfaces: the
// verify step that follows will fail against the same database, and the caller
// gets a 500 from there rather than a 401 that lies.
//
// The account row is returned alongside the id, zero-valued when there is none.
// It is the row the lookup already read, so carrying it out costs nothing, and
// it saves [Service.CreateFirstOrganization] a second pre-tenant round trip to
// learn the display name it names a default workspace after. Nothing may read it
// before the password has verified — [Service.verifyCredential] is the only
// caller, and it returns it only on the success path.
func (s *Service) loginSubject(ctx context.Context, email string) (uuid.UUID, store.IdentityUser, loginCandidate) {
	absent := loginCandidate{salt: s.absentSalt, params: s.params}

	// A random id rather than uuid.Nil when there is no account, so the two
	// lookups that follow probe the primary key index the same way a real
	// lookup does, and so nothing downstream can special-case the zero value.
	userID := uuid.New()

	user, err := withoutTenant(ctx, s.store, store.ReasonLogin,
		func(ctx context.Context, q store.IdentityQuerier) (store.IdentityUser, error) {
			return q.FindUserByEmail(ctx, email)
		})
	if err == nil {
		userID = user.ID
	} else {
		user = store.IdentityUser{}
	}

	// Called unconditionally, including for the stand-in id. Returning early
	// above would make an unknown address cost one fewer database round trip
	// than a known one, which is a smaller oracle than a skipped derivation but
	// an oracle all the same — and one that shows up in the pre-tenant audit
	// log as a different sequence of reasons.
	// TestLoginDoesTheSameWorkWhateverIsWrong asserts the sequences match.
	params, err := withoutTenant(ctx, s.store, store.ReasonPasswordParams,
		func(ctx context.Context, q store.IdentityQuerier) (store.PasswordKDFParams, error) {
			return q.PasswordParams(ctx, userID)
		})
	if err != nil {
		// The account exists but has no password — an invited user who has not
		// accepted, or later an external-provider-only account. Indistinguish-
		// able from an unknown address from here on.
		return userID, user, absent
	}

	return userID, user, loginCandidate{
		salt: params.Salt,
		params: Argon2Params{
			MemoryKiB:   uint32(params.MemoryKib),  //nolint:gosec // CHECK-constrained positive in migration 00005
			Iterations:  uint32(params.Iterations), //nolint:gosec // CHECK-constrained positive in migration 00005
			Parallelism: uint8(params.Parallelism), //nolint:gosec // CHECK-constrained to 1..255 in migration 00005
			KeyLength:   uint32(params.KeyLength),  //nolint:gosec // CHECK-constrained positive in migration 00005
			SaltLength:  uint32(len(params.Salt)),  //nolint:gosec // the length of a bytea that is CHECK-constrained to >= 16
		},
	}
}

// derive runs one argon2id derivation under a bounded wait.
func (s *Service) derive(ctx context.Context, password string, salt []byte, params Argon2Params) ([]byte, error) {
	derivationCtx, cancel := context.WithTimeout(ctx, derivationBudget)
	defer cancel()

	key, err := s.deriver.Derive(derivationCtx, password, salt, params)
	if err != nil {
		return nil, fmt.Errorf("deriving the password key: %w", err)
	}

	return key, nil
}

// organizations reads the memberships of one subject through the pre-tenant
// path. This is the only source of a tenant id in the whole service.
func (s *Service) organizations(ctx context.Context, userID uuid.UUID) ([]Organization, error) {
	rows, err := withoutTenant(ctx, s.store, store.ReasonListOrganizations,
		func(ctx context.Context, q store.IdentityQuerier) ([]store.UserOrganization, error) {
			return q.ListUserOrganizations(ctx, userID)
		})
	if err != nil {
		return nil, fmt.Errorf("listing organizations: %w", err)
	}

	out := make([]Organization, 0, len(rows))
	for _, row := range rows {
		out = append(out, Organization{
			ID:   row.OrganizationID,
			Name: row.Name,
			Slug: row.Slug,
			Role: row.Role,
		})
	}

	return out, nil
}

// logFailedLogin records an attempt without recording who made it.
//
// No email, no user id, no password material, no hash. The operational
// questions a failed login has to answer are "how many, from where, how fast",
// and none of them need the address — while a log full of addresses is a
// credential-stuffing target list with timestamps. The rate limiter's counters
// are keyed by a peppered hash for the same reason.
//
// operation names which endpoint the credential was presented to, because since
// issue #34 there is more than one. Without it every wrong password on
// POST /api/v1/organizations would be indistinguishable from one on
// POST /auth/login, and an alert on the login-failure rate would silently be an
// alert on two endpoints at once. It is a closed set of literals from the two
// call sites, so it cannot carry attacker-controlled text, and it discloses
// nothing: this is a server-side log, not a response, and the *response* still
// has to be identical for every failing case.
func (s *Service) logFailedLogin(ctx context.Context, operation, reason, clientIP string) {
	s.logger.InfoContext(ctx, "credential presentation failed",
		slog.String("event", "auth.login.failed"),
		slog.String("operation", operation),
		slog.String("reason", reason),
		slog.String("client_ip", clientIP),
	)
}

func findOrganization(orgs []Organization, id uuid.UUID) (Organization, bool) {
	for _, org := range orgs {
		if org.ID == id {
			return org, true
		}
	}

	return Organization{}, false
}

// validateEmailAddress is the one place an address is judged, so that
// validation and storage cannot disagree about what is acceptable.
//
// The rule is not chosen here; it is copied from the column. `users.email` in
// migrations/00002_tenancy.sql carries
//
//	CHECK (position('@' IN email) > 1)
//
// which is 1-indexed, so it demands at least one character before the `@` --
// Go index >= 1. The previous check asked only that an `@` appear *somewhere*,
// which is strictly looser, and the gap between the two was reachable:
// "@example.com" passed validation, reached the INSERT, violated the
// constraint, and fell through writeAuthError's mapping to a 500. A wrong
// value the user can fix was reported as a server fault.
//
// # What this deliberately does NOT do
//
// It does not try to be an address validator. "a@" still passes here and still
// passes the column, and that is not an oversight: the only thing that
// establishes an address is usable is sending mail to it, and a stricter Go
// rule than the column would recreate exactly the disagreement this function
// exists to remove -- in the safer direction, but with two sources of truth
// again. If addresses need to be real, that is address confirmation, not a
// tighter regexp.
//
// The invariant to preserve: this may be as strict as the column, never looser.
func validateEmailAddress(email string) error {
	switch {
	case email == "":
		return fmt.Errorf("%w: email is required", ErrInvalidInput)
	case len(email) > maxEmailLength:
		return fmt.Errorf("%w: email is not a valid address", ErrInvalidInput)
	case strings.Index(email, "@") < 1:
		return fmt.Errorf("%w: email is not a valid address", ErrInvalidInput)
	default:
		return nil
	}
}

func validateRegistration(email, password, displayName, organizationName string) error {
	if err := validateEmailAddress(email); err != nil {
		return err
	}

	if err := validateWorkspaceName(organizationName); err != nil {
		return err
	}

	switch {
	case displayName == "" || utf8.RuneCountInString(displayName) > maxDisplayNameLength:
		return fmt.Errorf("%w: display name must be 1-%d characters", ErrInvalidInput, maxDisplayNameLength)
	case len(password) < MinPasswordLength:
		return fmt.Errorf("%w: password must be at least %d characters", ErrInvalidInput, MinPasswordLength)
	case len(password) > MaxPasswordLength:
		return fmt.Errorf("%w: password must be at most %d characters", ErrInvalidInput, MaxPasswordLength)
	default:
		return nil
	}
}
