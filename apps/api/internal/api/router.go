// Package api wires the HTTP surface of the service: the Gin engine, shared
// middleware, and the handlers. Dependencies arrive through NewRouter; nothing
// in this package holds package-level state.
package api

import (
	"log/slog"

	"github.com/gin-gonic/gin"
)

// NewRouter builds the Gin engine with the service's middleware and routes.
//
// Auth is optional so that a build without it — the health-only configuration
// the tests for /healthz use — still produces a working engine rather than a
// nil dereference on the first request. In cmd/api it is always supplied.
func NewRouter(logger *slog.Logger, deps HealthDeps, authDeps AuthDeps) *gin.Engine {
	router := gin.New()

	// Gin trusts X-Forwarded-For from every peer by default, which makes
	// ClientIP() attacker-controlled and the per-address login budget
	// trivially bypassable — one header per attempt and every attempt looks
	// like a new client. Trusting nobody makes ClientIP() the peer address,
	// which is correct today (nothing sits in front of this) and will be wrong
	// the moment an ALB does. Filed as an issue rather than guessed at here,
	// because the right answer is the load balancer's subnet and that does not
	// exist yet.
	//
	// The error is impossible for a nil argument, but ignoring it silently
	// would hide a future typo.
	if err := router.SetTrustedProxies(nil); err != nil {
		logger.Error("configuring trusted proxies", slog.Any("error", err))
	}

	// gin.Logger writes unstructured text to stdout, which would violate the
	// structured-logging standard; requestLogger replaces it.
	router.Use(requestLogger(logger), recovery(logger))

	router.GET("/healthz", healthHandler(logger, deps))

	if authDeps.Service == nil || authDeps.Verifier == nil {
		return router
	}

	v1 := router.Group("/api/v1")

	// Unauthenticated. Each one is a credential-presentation endpoint, which is
	// why the rate limiter lives inside internal/auth rather than as middleware
	// here: it has to key on the account being attempted, and only the service
	// knows how to normalise that.
	v1.POST("/auth/register", registerHandler(logger, authDeps.Service))
	v1.POST("/auth/login", loginHandler(logger, authDeps.Service))
	v1.POST("/auth/refresh", refreshHandler(logger, authDeps.Service))
	v1.POST("/auth/logout", logoutHandler(logger, authDeps.Service))

	// Everything below requires a valid access token, and takes its tenant from
	// that token's org claim. There is no route parameter for an organization
	// anywhere in this tree — see auth_middleware.go for why that is a design
	// decision rather than an omission.
	authenticated := v1.Group("", requireAuth(logger, authDeps.Verifier))
	authenticated.GET("/me", meHandler(logger, authDeps.Service))
	authenticated.POST("/auth/organization", switchOrganizationHandler(logger, authDeps.Service))

	if authDeps.Store != nil {
		authenticated.GET("/members", membersHandler(logger, authDeps.Store))
	}

	return router
}
