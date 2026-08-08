package api

// The middleware's rejection cases, at HTTP level.
//
// Issue #8's acceptance criterion is "requests with no/expired/tampered tokens
// are rejected with correct status codes", so each of those is a row here with
// the status asserted rather than described. The positive case is one row among
// them, which is the right proportion: an authenticating middleware is mostly a
// list of things it refuses.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

const (
	middlewareSecret   = "0123456789abcdef0123456789abcdef"
	middlewareOther    = "fedcba9876543210fedcba9876543210"
	middlewareIssuer   = "collabboard-test"
	middlewareAudience = "collabboard-api-test"
)

func testIssuer(t *testing.T) *auth.Issuer {
	t.Helper()

	issuer, err := auth.NewIssuer(auth.TokenConfig{
		Secret:    []byte(middlewareSecret),
		Issuer:    middlewareIssuer,
		Audience:  middlewareAudience,
		AccessTTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("building the issuer: %v", err)
	}

	return issuer
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// protectedRouter is a minimal engine with one route behind requireAuth, which
// echoes the principal the middleware attached. Echoing it is what makes "the
// tenant came from the token" observable from outside.
func protectedRouter(t *testing.T, verifier TokenVerifier) *gin.Engine {
	t.Helper()

	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/protected", requireAuth(discardLogger(), verifier), func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "no principal"})

			return
		}

		fromContext, inContext := PrincipalFromContext(c.Request.Context())
		if !inContext || fromContext.TenantID != principal.TenantID {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "principal missing from the request context"})

			return
		}

		c.JSON(http.StatusOK, gin.H{
			"user_id":   principal.UserID.String(),
			"tenant_id": principal.TenantID.String(),
			"role":      principal.Role,
		})
	})

	return router
}

func TestRequireAuthRejectionCases(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	router := protectedRouter(t, issuer)

	principal := auth.Principal{
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		Role:      "member",
		SessionID: uuid.New(),
	}

	valid, _, err := issuer.Issue(principal)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	for _, tc := range []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "no Authorization header", header: "", wantStatus: http.StatusUnauthorized},
		{name: "not a bearer scheme", header: "Basic dXNlcjpwYXNz", wantStatus: http.StatusUnauthorized},
		{name: "bearer with no token", header: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "bearer with whitespace only", header: "Bearer    ", wantStatus: http.StatusUnauthorized},
		{name: "not a jwt", header: "Bearer nonsense", wantStatus: http.StatusUnauthorized},
		{
			name:       "tampered signature",
			header:     "Bearer " + tamperSignaturePart(t, valid),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "signed with a different key",
			header:     "Bearer " + signedWithOtherKey(t, principal),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "expired",
			header:     "Bearer " + expiredToken(t, principal),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "valid",
			header:     "Bearer " + valid,
			wantStatus: http.StatusOK,
		},
		{
			// RFC 7235 makes the scheme case-insensitive, and a client sending
			// "bearer" is not an attacker.
			name:       "valid, lowercase scheme",
			header:     "bearer " + valid,
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil)

			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}

			router.ServeHTTP(rec, req)

			t.Logf("%s -> %d %s", tc.name, rec.Code, strings.TrimSpace(rec.Body.String()))

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if rec.Code == http.StatusUnauthorized {
				// A 401 without WWW-Authenticate is a protocol violation, and
				// it is how a client knows to refresh rather than re-prompt.
				if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer ") {
					t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
				}

				// Every rejection says the same thing. "expired" versus "bad
				// signature" is useful in a log and is an oracle in a response.
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decoding the error body: %v", err)
				}

				if body["error"] != "authentication required" {
					t.Errorf("error body = %q; rejections must be indistinguishable to a client", body["error"])
				}
			}
		})
	}
}

// TestTheMiddlewarePassesTheTokensTenantThroughUnchanged is the positive half
// of the BOLA claim: whatever the token says is what the handler sees.
func TestTheMiddlewarePassesTheTokensTenantThroughUnchanged(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	router := protectedRouter(t, issuer)

	principal := auth.Principal{
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		Role:      "admin",
		SessionID: uuid.New(),
	}

	token, _, err := issuer.Issue(principal)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	router.ServeHTTP(rec, req)

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}

	t.Logf("handler saw user=%s tenant=%s role=%s", body["user_id"], body["tenant_id"], body["role"])

	if body["tenant_id"] != principal.TenantID.String() {
		t.Errorf("handler saw tenant %s, want %s", body["tenant_id"], principal.TenantID)
	}

	if body["user_id"] != principal.UserID.String() {
		t.Errorf("handler saw user %s, want %s", body["user_id"], principal.UserID)
	}
}

// TestAPrincipalCannotBeForgedIntoAContext is a structural claim rather than a
// behavioural one: the context key is an unexported type, so a package outside
// internal/api cannot construct it and inject a principal. This asserts the
// half that *is* testable — a foreign value under a same-named key is ignored.
func TestAPrincipalCannotBeForgedIntoAContext(t *testing.T) {
	t.Parallel()

	type lookalike struct{}

	ctx := context.WithValue(t.Context(), lookalike{}, auth.Principal{
		UserID:   uuid.New(),
		TenantID: uuid.New(),
	})

	principal, ok := PrincipalFromContext(ctx)

	t.Logf("a principal stored under a look-alike key -> found=%t %+v", ok, principal)

	if ok {
		t.Error("PrincipalFromContext accepted a value stored under a different key type")
	}
}

// TestAZeroPrincipalIsRefused guards the case where a handler runs without the
// middleware: a zero principal carries uuid.Nil, which is a syntactically valid
// tenant that matches no organization — so it would read as "no data" instead
// of "no authentication".
func TestAZeroPrincipalIsRefused(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "nothing set", value: nil},
		{name: "wrong type", value: "not a principal"},
		{name: "zero principal", value: auth.Principal{}},
		{name: "principal with no tenant", value: auth.Principal{UserID: uuid.New()}},
		{name: "principal with no subject", value: auth.Principal{TenantID: uuid.New()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if tc.value != nil {
				c.Set(principalKey, tc.value)
			}

			_, ok := principalFrom(c)

			t.Logf("%s -> found=%t", tc.name, ok)

			if ok {
				t.Error("principalFrom accepted a principal that cannot name a tenant")
			}
		})
	}
}

func tamperSignaturePart(t *testing.T, token string) string {
	t.Helper()

	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[2]) < 4 {
		t.Fatalf("token is not a signed jwt: %q", token)
	}

	sig := []byte(parts[2])

	mid := len(sig) / 2
	if sig[mid] == 'A' {
		sig[mid] = 'B'
	} else {
		sig[mid] = 'A'
	}

	return parts[0] + "." + parts[1] + "." + string(sig)
}

func signedWithOtherKey(t *testing.T, p auth.Principal) string {
	t.Helper()

	return signClaims(t, []byte(middlewareOther), middlewareClaims(p, time.Now().Add(time.Hour)))
}

func expiredToken(t *testing.T, p auth.Principal) string {
	t.Helper()

	return signClaims(t, []byte(middlewareSecret), middlewareClaims(p, time.Now().Add(-time.Minute)))
}

func middlewareClaims(p auth.Principal, expires time.Time) auth.Claims {
	issued := time.Now().Add(-time.Hour)

	return auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			Issuer:    middlewareIssuer,
			Audience:  jwt.ClaimStrings{middlewareAudience},
			IssuedAt:  jwt.NewNumericDate(issued),
			NotBefore: jwt.NewNumericDate(issued),
			ExpiresAt: jwt.NewNumericDate(expires),
			ID:        uuid.NewString(),
		},
		OrganizationID: p.TenantID.String(),
		Role:           p.Role,
		SessionID:      p.SessionID.String(),
	}
}

func signClaims(t *testing.T, key []byte, claims auth.Claims) string {
	t.Helper()

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	return signed
}
