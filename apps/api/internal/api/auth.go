package api

// The auth HTTP surface.
//
// Five unauthenticated endpoints' worth of attack surface is the whole reason
// this file is short and boring. Handlers do three things: decode, call
// internal/auth, and map an error to a status. No branching on identity, no
// tenant plumbing, no SQL.
//
// Note what is *not* here: any route parameter, header or body field naming an
// organization, except the one on POST /auth/organization — which is the
// deliberate exception, is authenticated, and is checked against the subject's
// memberships before it can become a tenant context. See auth_middleware.go.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// AuthService is the slice of internal/auth the HTTP layer uses.
type AuthService interface {
	Register(ctx context.Context, in auth.RegisterInput) (auth.RegisterResult, error)
	Login(ctx context.Context, in auth.LoginInput) (auth.LoginResult, error)
	Refresh(ctx context.Context, refreshToken string) (auth.LoginResult, error)
	Logout(ctx context.Context, refreshToken string) error
	SwitchOrganization(ctx context.Context, principal auth.Principal, target uuid.UUID) (auth.LoginResult, error)
	Organizations(ctx context.Context, principal auth.Principal) ([]auth.Organization, error)
}

// TenantStore is the slice of internal/store the authenticated routes use.
//
// One method, and it is the one that takes a tenant id. Passing the id
// explicitly rather than reading it from a context is deliberate: it means the
// call site has to name where the tenant came from, and every call site in this
// package names a principal.
type TenantStore interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error
}

// AuthDeps are the dependencies the auth routes need.
type AuthDeps struct {
	Service  AuthService
	Verifier TokenVerifier
	Store    TenantStore
}

// errorResponse is the single error shape this API returns. One field, so a
// client cannot come to depend on a "reason" that a future version stops
// distinguishing.
type errorResponse struct {
	Error string `json:"error"`
}

// messageInternalError is the body of every 500 this package writes. One
// constant, because the one thing a 500 must never do is describe what went
// wrong: the detail goes to the log with the request id, and the client gets a
// sentence.
const messageInternalError = "internal server error"

type registerRequest struct {
	Email            string `json:"email"            binding:"required"`
	Password         string `json:"password"         binding:"required"`
	DisplayName      string `json:"display_name"     binding:"required"`
	OrganizationName string `json:"organization_name"`
}

type registerResponse struct {
	UserID       string           `json:"user_id"`
	Email        string           `json:"email"`
	DisplayName  string           `json:"display_name"`
	Organization organizationBody `json:"organization"`
}

type organizationBody struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Role string `json:"role,omitempty"`
}

type loginRequest struct {
	Email    string `json:"email"    binding:"required"`
	Password string `json:"password" binding:"required"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type switchOrganizationRequest struct {
	OrganizationID string `json:"organization_id" binding:"required"`
}

// sessionResponse is what login, refresh and organization switch return.
//
// The refresh token is in the body rather than a Set-Cookie because the client
// is a separate origin (Next.js on another host in dev, another domain in
// production) and a cookie-based session would need CSRF defences this service
// does not have yet. Flagged in the PR as an invented decision: it is the right
// call for a token-based SPA and the wrong one if the web app ever becomes
// same-origin server-rendered.
type sessionResponse struct {
	TokenType    string           `json:"token_type"`
	AccessToken  string           `json:"access_token"`
	ExpiresIn    int              `json:"expires_in"`
	RefreshToken string           `json:"refresh_token"`
	UserID       string           `json:"user_id"`
	Organization organizationBody `json:"organization"`
}

type meResponse struct {
	UserID        string             `json:"user_id"`
	Role          string             `json:"role"`
	SessionID     string             `json:"session_id"`
	Organization  organizationBody   `json:"organization"`
	Organizations []organizationBody `json:"organizations"`
}

type memberBody struct {
	MembershipID string `json:"membership_id"`
	UserID       string `json:"user_id"`
	Email        string `json:"email"`
	DisplayName  string `json:"display_name"`
	Role         string `json:"role"`
	JoinedAt     string `json:"joined_at"`
}

// registerHandler creates an account, its credential and its first workspace.
//
// A duplicate address gets 409. That does disclose whether an address is
// registered, and it is a deliberate, narrower trade than the one login makes:
// the alternative — always answer 201 and confirm by email — needs a mailer
// this service does not have, and returning 201 without one would leave a user
// unable to tell a typo from a taken address. Login, which is the endpoint an
// attacker actually enumerates against at scale, gives nothing away. The
// registration endpoint is rate limited by the same per-address budget.
func registerHandler(logger *slog.Logger, service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req registerRequest
		if !bindJSON(c, &req) {
			return
		}

		result, err := service.Register(c.Request.Context(), auth.RegisterInput{
			Email:            req.Email,
			Password:         req.Password,
			DisplayName:      req.DisplayName,
			OrganizationName: req.OrganizationName,
		})
		if err != nil {
			writeAuthError(c, logger, err)

			return
		}

		c.JSON(http.StatusCreated, registerResponse{
			UserID:      result.UserID.String(),
			Email:       result.Email,
			DisplayName: result.DisplayName,
			Organization: organizationBody{
				ID:   result.OrganizationID.String(),
				Name: result.OrganizationName,
				Slug: result.OrganizationSlug,
				Role: auth.RoleOwner,
			},
		})
	}
}

// loginHandler exchanges a password for a session.
func loginHandler(logger *slog.Logger, service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req loginRequest
		if !bindJSON(c, &req) {
			return
		}

		result, err := service.Login(c.Request.Context(), auth.LoginInput{
			Email:    req.Email,
			Password: req.Password,
			ClientIP: c.ClientIP(),
		})
		if err != nil {
			writeAuthError(c, logger, err)

			return
		}

		c.JSON(http.StatusOK, newSessionResponse(result))
	}
}

// refreshHandler rotates a refresh token and mints a new access token.
func refreshHandler(logger *slog.Logger, service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req refreshRequest
		if !bindJSON(c, &req) {
			return
		}

		result, err := service.Refresh(c.Request.Context(), req.RefreshToken)
		if err != nil {
			writeAuthError(c, logger, err)

			return
		}

		c.JSON(http.StatusOK, newSessionResponse(result))
	}
}

// logoutHandler revokes a session.
//
// Unauthenticated on purpose: a client whose access token has already expired
// still has to be able to log out, and the refresh token is itself the
// credential. An unknown token is 204, not 404 — see SessionStore.Revoke.
func logoutHandler(logger *slog.Logger, service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req refreshRequest
		if !bindJSON(c, &req) {
			return
		}

		if err := service.Logout(c.Request.Context(), req.RefreshToken); err != nil {
			writeAuthError(c, logger, err)

			return
		}

		c.Status(http.StatusNoContent)
	}
}

// switchOrganizationHandler is the one endpoint that takes an organization id
// from a client.
//
// It is authenticated, and the id it receives is checked against the
// *authenticated subject's* memberships before it becomes anything. A
// non-member gets 403 and no token, and the requested id never reaches
// store.WithTenant on that path. This is the endpoint auth_bola_test.go attacks
// hardest, because it is the only one where the attack is even coherent.
func switchOrganizationHandler(logger *slog.Logger, service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			rejectUnauthenticated(c, logger, "no_principal", nil)

			return
		}

		var req switchOrganizationRequest
		if !bindJSON(c, &req) {
			return
		}

		target, err := uuid.Parse(req.OrganizationID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: "organization_id must be a uuid"})

			return
		}

		result, err := service.SwitchOrganization(c.Request.Context(), principal, target)
		if err != nil {
			writeAuthError(c, logger, err)

			return
		}

		c.JSON(http.StatusOK, newSessionResponse(result))
	}
}

// meHandler reports the authenticated principal and the organizations it could
// act in.
//
// The organization list comes from the principal's user id, never from a
// parameter — the underlying pre-tenant function does not authorize, so passing
// an id from a request would turn this into a membership-disclosure endpoint.
// ADR 0002 says so explicitly, and this is the handler it was talking about.
func meHandler(logger *slog.Logger, service AuthService) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			rejectUnauthenticated(c, logger, "no_principal", nil)

			return
		}

		organizations, err := service.Organizations(c.Request.Context(), principal)
		if err != nil {
			writeAuthError(c, logger, err)

			return
		}

		body := meResponse{
			UserID:        principal.UserID.String(),
			Role:          principal.Role,
			SessionID:     principal.SessionID.String(),
			Organizations: make([]organizationBody, 0, len(organizations)),
		}

		for _, org := range organizations {
			rendered := organizationBody{
				ID:   org.ID.String(),
				Name: org.Name,
				Slug: org.Slug,
				Role: org.Role,
			}

			body.Organizations = append(body.Organizations, rendered)

			if org.ID == principal.TenantID {
				body.Organization = rendered
			}
		}

		c.JSON(http.StatusOK, body)
	}
}

// membersHandler is the proof that the tenant flows from the token into the
// data layer without a handler ever choosing it.
//
// It exists in this PR for that reason and no other: board and card CRUD are
// out of scope for #8, but "tenant context flows automatically from the token
// to the store layer" is an acceptance criterion, and a criterion with no
// endpoint behind it is untested. Members is the right choice because it is
// auth-adjacent rather than product surface, and because the two-tenant fixture
// shares a user between organizations — so "tenant A sees only its own members"
// is a claim that can fail.
func membersHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			rejectUnauthenticated(c, logger, "no_principal", nil)

			return
		}

		var rows []store.ListMembersRow

		// principal.TenantID, and there is no other expression in this package
		// that could appear here. Grep for WithTenant: this is the only call.
		err := tenantStore.WithTenant(c.Request.Context(), principal.TenantID,
			func(ctx context.Context, q store.Querier) error {
				var qerr error

				rows, qerr = q.ListMembers(ctx)

				return qerr
			})
		if err != nil {
			logger.ErrorContext(c.Request.Context(), "listing members failed",
				slog.String("event", "members.list.failed"),
				slog.String("tenant_id", principal.TenantID.String()),
				slog.Any("error", err),
			)

			c.AbortWithStatusJSON(http.StatusInternalServerError, errorResponse{Error: messageInternalError})

			return
		}

		members := make([]memberBody, 0, len(rows))
		for _, row := range rows {
			members = append(members, memberBody{
				MembershipID: row.MembershipID.String(),
				UserID:       row.UserID.String(),
				Email:        row.Email,
				DisplayName:  row.DisplayName,
				Role:         row.Role,
				JoinedAt:     row.JoinedAt.UTC().Format(time.RFC3339),
			})
		}

		c.JSON(http.StatusOK, gin.H{"members": members})
	}
}

func newSessionResponse(result auth.LoginResult) sessionResponse {
	return sessionResponse{
		TokenType:    "Bearer",
		AccessToken:  result.Tokens.AccessToken,
		ExpiresIn:    int(result.Tokens.ExpiresIn.Seconds()),
		RefreshToken: result.Tokens.RefreshToken,
		UserID:       result.Principal.UserID.String(),
		Organization: organizationBody{
			ID:   result.Organization.ID.String(),
			Name: result.Organization.Name,
			Slug: result.Organization.Slug,
			Role: result.Organization.Role,
		},
	}
}

// bindJSON decodes a request body, writing a 400 and returning false on
// failure. The binding error is not echoed: it can quote the submitted body,
// which for these endpoints means quoting a password back at whoever sent it —
// and into whatever log or error tracker sits in front of the client.
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: "request body is not valid"})

		return false
	}

	return true
}

// writeAuthError maps the auth package's errors onto statuses.
//
// The default is 500 with a generic body and the real error in the log, so a
// wrapped database error cannot become a response. Everything that *is*
// mapped is mapped to a message that says nothing about stored state:
// "invalid email or password" is the same answer for an unknown address and a
// wrong password, because the layer below already made them the same error.
func writeAuthError(c *gin.Context, logger *slog.Logger, err error) {
	ctx := c.Request.Context()

	var limited *auth.RateLimitError

	switch {
	case errors.As(err, &limited):
		c.Header("Retry-After", strconv.Itoa(int(limited.RetryAfter.Round(time.Second).Seconds())))
		c.AbortWithStatusJSON(http.StatusTooManyRequests,
			errorResponse{Error: "too many attempts, try again later"})

	case errors.Is(err, auth.ErrInvalidCredentials):
		c.AbortWithStatusJSON(http.StatusUnauthorized, errorResponse{Error: "invalid email or password"})

	case errors.Is(err, auth.ErrRefreshUnknown), errors.Is(err, auth.ErrRefreshReused):
		// One status and one message for both. A client cannot be told "that
		// token was already used" without also being told the token was real.
		c.Header("WWW-Authenticate", `Bearer realm="collabboard", error="invalid_token"`)
		c.AbortWithStatusJSON(http.StatusUnauthorized, errorResponse{Error: "session is no longer valid"})

	case errors.Is(err, auth.ErrEmailTaken):
		c.AbortWithStatusJSON(http.StatusConflict, errorResponse{Error: "email is already registered"})

	case errors.Is(err, auth.ErrInvalidInput):
		// Safe to show: it describes the submitted values, not stored state.
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: err.Error()})

	case errors.Is(err, auth.ErrNoOrganization):
		c.AbortWithStatusJSON(http.StatusForbidden,
			errorResponse{Error: "this account does not belong to an organization"})

	case errors.Is(err, auth.ErrNotAMember):
		c.AbortWithStatusJSON(http.StatusForbidden,
			errorResponse{Error: "not a member of that organization"})

	default:
		logger.ErrorContext(ctx, "auth request failed",
			slog.String("event", "auth.request.failed"),
			slog.String("path", c.FullPath()),
			slog.Any("error", err),
		)

		c.AbortWithStatusJSON(http.StatusInternalServerError, errorResponse{Error: messageInternalError})
	}
}
