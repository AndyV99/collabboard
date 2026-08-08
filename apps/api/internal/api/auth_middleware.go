package api

// The authentication middleware, and the tenant boundary.
//
// # Why this file is the security-critical one
//
// internal/store's WithTenant sets app.tenant_id to whatever it is handed and
// Postgres serves that tenant faithfully. ADR 0001 says so in as many words:
// that layer is isolation, not authorization. Which means the entire defence
// against a user of organization A reading organization B's data is the
// question "where did the tenant id come from", and this file is the only
// place in the service that answers it for a request.
//
// The answer is: from the "org" claim of a token this service signed, and from
// nowhere else.
//
// # What that rules out
//
// There is no code path here — or anywhere in internal/api — that reads a
// tenant from:
//
//   - a request header (X-Tenant-ID, X-Organization-ID, or any other);
//   - a path segment (/organizations/:id/...);
//   - a query parameter (?org=);
//   - a request body field.
//
// A client may send any of those. They are ignored: [Principal] is built from
// the verified claims and nothing merges request data into it afterwards.
// Changing organization is a separate, authenticated operation
// (POST /api/v1/auth/organization) that re-checks membership through the
// pre-tenant path and issues a *new token* — so even the one endpoint that
// takes an organization id from a client cannot turn that id into a tenant
// context without a membership.
//
// auth_bola_test.go asserts this by attacking it: an authenticated member of
// tenant A sends tenant B's id through every one of those channels and reads
// tenant A's data every time, and the same test shows the assertion failing
// when the middleware is bypassed.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// principalContextKey is the key the principal is stored under.
//
// An unexported type, so no package outside internal/api can construct the key
// and inject a principal into a request context. That is not paranoia about
// other packages in this repository; it is what makes "the principal came from
// the middleware" a property of the type system rather than of a convention.
type principalContextKey struct{}

// principalKey is the gin context key, for handlers that have the *gin.Context
// rather than the request context.
const principalKey = "collabboard.principal"

// TokenVerifier is the middleware's dependency: turn a bearer token into a
// principal, or say why not.
type TokenVerifier interface {
	Verify(token string) (auth.Principal, error)
}

// requireAuth rejects any request without a valid access token, and attaches
// the principal it describes to the request.
//
// Every rejection is a 401 with the same body. The reason is logged, never
// returned: "expired" and "bad signature" are useful to an operator and are an
// oracle for a client probing what this service accepts.
func requireAuth(logger *slog.Logger, verifier TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c.GetHeader("Authorization"))
		if !ok {
			rejectUnauthenticated(c, logger, "missing_bearer_token", nil)

			return
		}

		principal, err := verifier.Verify(token)
		if err != nil {
			rejectUnauthenticated(c, logger, tokenRejectionReason(err), err)

			return
		}

		// The tenant is the claim, full stop. Nothing below this line consults
		// the request for one, and there is no setter a handler could call.
		c.Set(principalKey, principal)
		c.Request = c.Request.WithContext(
			context.WithValue(c.Request.Context(), principalContextKey{}, principal))

		c.Next()
	}
}

// bearerToken extracts the credential from an Authorization header.
//
// The scheme is compared case-insensitively because RFC 7235 says it is
// case-insensitive, and a client sending "bearer" is not an attacker.
func bearerToken(header string) (string, bool) {
	const scheme = "bearer "

	if len(header) <= len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}

	token := strings.TrimSpace(header[len(scheme):])

	return token, token != ""
}

// tokenRejectionReason maps a verification failure to a stable label for logs
// and metrics. Deliberately a closed set: it ends up in a log field, so it must
// not be able to carry attacker-controlled text.
func tokenRejectionReason(err error) string {
	switch {
	case errors.Is(err, auth.ErrTokenExpired):
		return "expired"
	case errors.Is(err, auth.ErrTokenMalformed):
		return "malformed"
	default:
		return "invalid"
	}
}

// rejectUnauthenticated writes the one 401 this service produces.
//
// WWW-Authenticate is set because 401 without it is a protocol violation and
// because it is how a client knows to refresh rather than to re-prompt. The
// error code in it is coarse on purpose: RFC 6750 allows invalid_token and
// that is all a client needs.
func rejectUnauthenticated(c *gin.Context, logger *slog.Logger, reason string, err error) {
	attrs := []slog.Attr{
		slog.String("event", "auth.token.rejected"),
		slog.String("reason", reason),
		slog.String("path", c.FullPath()),
		slog.String("client_ip", c.ClientIP()),
	}

	// The error is logged as a category, never as text, and the token itself is
	// never logged at all — not even truncated. A prefix of a JWT is its header
	// and the start of its payload, which is exactly the part worth redacting.
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}

	logger.LogAttrs(c.Request.Context(), slog.LevelInfo, "authentication failed", attrs...)

	c.Header("WWW-Authenticate", `Bearer realm="collabboard", error="invalid_token"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, errorResponse{Error: "authentication required"})
}

// principalFrom returns the principal the middleware attached.
//
// The second return value is false when there is none, which for a route behind
// requireAuth cannot happen — but a handler that assumed it and dereferenced a
// zero principal would run with uuid.Nil as its tenant, which is a syntactically
// valid tenant matching no organization. That would read as "no data" rather
// than "no principal". Refusing is the cheaper failure.
func principalFrom(c *gin.Context) (auth.Principal, bool) {
	value, ok := c.Get(principalKey)
	if !ok {
		return auth.Principal{}, false
	}

	principal, ok := value.(auth.Principal)
	if !ok || principal.UserID == uuid.Nil || principal.TenantID == uuid.Nil {
		return auth.Principal{}, false
	}

	return principal, true
}

// PrincipalFromContext returns the principal attached to a request context.
//
// Exported for code that has a context.Context rather than a *gin.Context — the
// WebSocket hub in #9 will. The key is unexported, so this is the only way in
// and there is no way to put one there from outside this package.
func PrincipalFromContext(ctx context.Context) (auth.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(auth.Principal)
	if !ok || principal.UserID == uuid.Nil || principal.TenantID == uuid.Nil {
		return auth.Principal{}, false
	}

	return principal, true
}
