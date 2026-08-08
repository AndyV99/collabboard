package api

// The subprotocol credential adapter, and the routing of the two realtime
// endpoints.
//
// The adapter is small and it touches the Authorization header, which makes it
// exactly the kind of thing that deserves more test than it has code.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// bearerEchoRouter mounts the adapter in front of the real middleware and
// echoes the principal, so "which token was believed" is observable.
func bearerEchoRouter(t *testing.T, verifier TokenVerifier) *gin.Engine {
	t.Helper()

	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.GET("/api/v1/ws", websocketBearer(), requireAuth(discardLogger(), verifier), func(c *gin.Context) {
		principal, ok := principalFrom(c)
		if !ok {
			c.AbortWithStatus(http.StatusInternalServerError)

			return
		}

		c.JSON(http.StatusOK, gin.H{"user_id": principal.UserID.String(), "org": principal.TenantID.String()})
	})

	return router
}

func TestASubprotocolBorneTokenIsVerifiedLikeAnyOther(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	router := bearerEchoRouter(t, issuer)

	principal := auth.Principal{
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		Role:      "member",
		SessionID: uuid.New(),
	}

	token, _, err := issuer.Issue(principal)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	for _, tc := range []struct {
		name     string
		protocol string
		want     int
	}{
		{
			name:     "the browser form: the real subprotocol plus the credential one",
			protocol: "collabboard.v1, " + bearerSubprotocolPrefix + token,
			want:     http.StatusOK,
		},
		{
			name:     "credential first",
			protocol: bearerSubprotocolPrefix + token + ",collabboard.v1",
			want:     http.StatusOK,
		},
		{
			name:     "no credential subprotocol at all",
			protocol: "collabboard.v1",
			want:     http.StatusUnauthorized,
		},
		{
			name:     "the prefix with nothing after it",
			protocol: "collabboard.v1, " + bearerSubprotocolPrefix,
			want:     http.StatusUnauthorized,
		},
		{
			name:     "a token under some other prefix is not lifted",
			protocol: "collabboard.v1, access_token." + token,
			want:     http.StatusUnauthorized,
		},
		{
			name:     "not a token",
			protocol: "collabboard.v1, " + bearerSubprotocolPrefix + "nonsense",
			want:     http.StatusUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ws", nil)
			req.Header.Set("Sec-WebSocket-Protocol", tc.protocol)
			router.ServeHTTP(rec, req)

			t.Logf("%s -> %d %s", tc.name, rec.Code, rec.Body.String())

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}

			if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), principal.TenantID.String()) {
				t.Errorf("the principal is not the one the token names: %s", rec.Body.String())
			}
		})
	}
}

// TestTheSubprotocolCannotOverrideAnAuthorizationHeader is the rule that keeps
// this from being a second credential path rather than a second transport for
// the same one.
func TestTheSubprotocolCannotOverrideAnAuthorizationHeader(t *testing.T) {
	t.Parallel()

	issuer := testIssuer(t)
	router := bearerEchoRouter(t, issuer)

	header := auth.Principal{UserID: uuid.New(), TenantID: uuid.New(), Role: "member", SessionID: uuid.New()}
	subprotocol := auth.Principal{UserID: uuid.New(), TenantID: uuid.New(), Role: "owner", SessionID: uuid.New()}

	headerToken, _, err := issuer.Issue(header)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	subprotocolToken, _, err := issuer.Issue(subprotocol)
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ws", nil)
	req.Header.Set("Authorization", "Bearer "+headerToken)
	req.Header.Set("Sec-WebSocket-Protocol", "collabboard.v1, "+bearerSubprotocolPrefix+subprotocolToken)
	router.ServeHTTP(rec, req)

	t.Logf("both credentials present -> %d %s", rec.Code, rec.Body.String())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), header.TenantID.String()) {
		t.Errorf("the subprotocol credential displaced the header one: %s", rec.Body.String())
	}

	if strings.Contains(rec.Body.String(), subprotocol.TenantID.String()) {
		t.Errorf("the subprotocol credential was believed: %s", rec.Body.String())
	}
}

// TestTheRealtimeRoutesAreOnlyMountedWhenSupplied covers the optional wiring,
// which is what keeps the health-only configuration from panicking.
func TestTheRealtimeRoutesAreOnlyMountedWhenSupplied(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	issuer := testIssuer(t)

	token, _, err := issuer.Issue(auth.Principal{
		UserID: uuid.New(), TenantID: uuid.New(), Role: "member", SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing: %v", err)
	}

	without := NewRouter(discardLogger(),
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
		AuthDeps{Service: &membershipService{issuer: issuer}, Verifier: issuer},
		RealtimeDeps{})

	for _, path := range []string{"/api/v1/ws", "/api/v1/boards/" + uuid.NewString() + "/events"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		without.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s without realtime deps -> %d, want 404", path, rec.Code)
		}
	}

	reached := false

	with := NewRouter(discardLogger(),
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
		AuthDeps{Service: &membershipService{issuer: issuer}, Verifier: issuer},
		RealtimeDeps{
			Connect: func(c *gin.Context) {
				reached = true

				c.Status(http.StatusOK)
			},
		})

	// Unauthenticated: the handler must not be reached at all.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ws", nil)
	with.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated upgrade -> %d, want 401", rec.Code)
	}

	if reached {
		t.Fatal("the websocket handler ran without authentication")
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	with.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || !reached {
		t.Errorf("an authenticated upgrade -> %d (handler reached: %t), want 200 and true", rec.Code, reached)
	}
}
