package api

// What an anonymous caller can reach, asserted as an inventory.
//
// # Why this exists
//
// Until issue #34 the anonymous surface was identifiable by path prefix: it was
// the four routes under /api/v1/auth. POST /api/v1/organizations broke that. It
// had to — an account with no organization cannot hold a token, so the endpoint
// that gives it one cannot sit behind requireAuth (see
// internal/auth/organizations.go and ADR 0009) — but it means "is this route
// public?" is no longer answerable by looking at the path, and both router.go
// and auth.go's file comments were written when it was.
//
// So the property is asserted instead of narrated. A route mounted on the
// unauthenticated group by mistake, or one that quietly stops requiring a token,
// now fails a test naming it, rather than waiting to be noticed in review.
//
// The check is behavioural rather than structural: it does not inspect Gin's
// middleware chain, it sends every registered route a request with no
// Authorization header and asks what comes back. That is the question an
// attacker asks, and it cannot be satisfied by a route that merely *looks*
// protected.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// publicRoutes is every method+path an anonymous caller is allowed to get a
// non-401 from.
//
// Adding to this map is the deliberate act. Five of the six are credential
// endpoints; /healthz is the liveness probe and has no body worth protecting.
var publicRoutes = map[string]string{
	"GET /healthz": "liveness and readiness, consumed by the load balancer",

	"POST /api/v1/auth/register": "creates an account; there is no caller yet",
	"POST /api/v1/auth/login":    "exchanges a password for a session",
	"POST /api/v1/auth/refresh":  "the refresh token is itself the credential",
	"POST /api/v1/auth/logout":   "an expired access token must still be able to log out",

	// The one that is not under /auth, and the reason this test exists.
	"POST /api/v1/organizations": "issue #34: an account with no organization has no token to present",
}

func TestOnlyTheKnownPublicRoutesAnswerWithoutAToken(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	router := NewRouter(discardLogger(), BodyLimits{},
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
		AuthDeps{
			Service:  &countingAuthService{},
			Verifier: testIssuer(t),
			Store:    newCRUDStore(),
		},
		RealtimeDeps{
			Connect:   func(c *gin.Context) { c.Status(http.StatusSwitchingProtocols) },
			Publisher: &recordingPublisher{},
		})

	routes := router.Routes()

	if len(routes) < 20 {
		t.Fatalf("only %d routes mounted; this test is not seeing the real surface", len(routes))
	}

	seenPublic := map[string]bool{}

	for _, route := range routes {
		key := route.Method + " " + route.Path

		rec := httptest.NewRecorder()

		// Every path parameter gets a syntactically valid uuid, so a route is
		// rejected for its missing token rather than for a malformed id.
		req := httptest.NewRequestWithContext(t.Context(), route.Method,
			concretePath(route.Path), strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")

		router.ServeHTTP(rec, req)

		_, isPublic := publicRoutes[key]

		switch {
		case isPublic && rec.Code == http.StatusUnauthorized:
			t.Errorf("%s is listed as public but answered 401 without a token", key)
		case isPublic:
			seenPublic[key] = true
		case rec.Code != http.StatusUnauthorized:
			t.Errorf("%s answered %d without a token, want 401.\n"+
				"Either it is missing requireAuth, or it is a new public route — in which case add "+
				"it to publicRoutes with the reason it is safe, and say so in the PR.",
				key, rec.Code)
		}
	}

	for key, why := range publicRoutes {
		if !seenPublic[key] {
			t.Errorf("publicRoutes lists %s (%s) but the router does not mount it; "+
				"the allow-list has outlived the route", key, why)
		}
	}

	t.Logf("%d routes mounted, %d of them reachable without a token", len(routes), len(seenPublic))
}

// concretePath replaces Gin's :params with uuids.
func concretePath(path string) string {
	segments := strings.Split(path, "/")

	for i, segment := range segments {
		if strings.HasPrefix(segment, ":") {
			segments[i] = uuid.NewString()
		}
	}

	return strings.Join(segments, "/")
}

// TestTheRouteInventoryAssertionHasTeeth is the companion every assertion in
// this package gets: proof that the check above fails when the property does.
//
// It mounts a route on the unauthenticated group the way a careless change
// would, and shows the same probe catching it.
func TestTheRouteInventoryAssertionHasTeeth(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	router := gin.New()
	v1 := router.Group("/api/v1")

	// Mounted with no requireAuth — the mistake this guards against.
	v1.GET("/projects", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"projects": []string{}}) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/projects", nil)

	router.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("a route mounted without requireAuth answered 401; the probe above proves nothing")
	}

	if _, listed := publicRoutes["GET /api/v1/projects"]; listed {
		t.Fatal("GET /api/v1/projects is in publicRoutes; the inventory would not catch this")
	}

	t.Logf("an unprotected GET /api/v1/projects answers %d, and is not in publicRoutes, "+
		"so the inventory test would fail on it", rec.Code)
}
