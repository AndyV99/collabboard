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
// Auth and realtime are optional so that a build without them — the health-only
// configuration the tests for /healthz use — still produces a working engine
// rather than a nil dereference on the first request. In cmd/api both are
// always supplied.
func NewRouter(logger *slog.Logger, deps HealthDeps, authDeps AuthDeps, realtimeDeps RealtimeDeps) *gin.Engine {
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

		mountBoardRoutes(authenticated, logger, authDeps.Store, realtimeDeps.Publisher)
	}

	// The WebSocket upgrade is authenticated by the *same* requireAuth as every
	// route above, with the same verifier. It is mounted outside the group only
	// so that websocketBearer can run first — see realtime.go for why a browser
	// needs it and why it is not a second credential.
	if realtimeDeps.Connect != nil {
		v1.GET("/ws", websocketBearer(), requireAuth(logger, authDeps.Verifier), realtimeDeps.Connect)
	}

	return router
}

// mountBoardRoutes mounts projects, boards, columns and cards.
//
// Every route is on the `authenticated` group, so requireAuth has already run
// and a principal exists before any handler does anything. There is no second
// authorization path: an object id in the path is resolved inside the caller's
// own tenant context and matches nothing when it belongs elsewhere. See crud.go.
//
// The write handlers additionally take the realtime publisher, and every one of
// them broadcasts *after* its transaction commits. Which writes announce
// themselves is a decision, not a default: everything whose effect is visible to
// someone already looking at the board does — cards and columns in full, board
// rename and delete — and everything else does not, because a realtime room is a
// board and there is no room to publish a project change or a board creation to.
// See events.go.
//
// The shape is deliberately flat rather than fully nested. A card lives at
// /cards/:card_id, not /projects/:p/boards/:b/columns/:c/cards/:id, because the
// longer form invites a handler to trust the ancestors in the path — and a
// client holding four ids can present three of them from one tenant and one from
// another. One id, resolved against the token's tenant, has no such seam. The
// nested forms that do exist are creates and lists, where the parent is the
// collection being addressed rather than a claim about the child.
func mountBoardRoutes(
	authenticated *gin.RouterGroup,
	logger *slog.Logger,
	tenantStore TenantStore,
	publisher EventPublisher,
) {
	authenticated.POST("/projects", createProjectHandler(logger, tenantStore))
	authenticated.GET("/projects", listProjectsHandler(logger, tenantStore))
	authenticated.GET("/projects/:project_id", getProjectHandler(logger, tenantStore))
	authenticated.PATCH("/projects/:project_id", patchProjectHandler(logger, tenantStore))
	authenticated.POST("/projects/:project_id/archive", archiveProjectHandler(logger, tenantStore))

	authenticated.POST("/projects/:project_id/boards", createBoardHandler(logger, tenantStore))
	authenticated.GET("/projects/:project_id/boards", listBoardsHandler(logger, tenantStore))
	authenticated.GET("/boards/:board_id", getBoardHandler(logger, tenantStore))
	authenticated.PATCH("/boards/:board_id", patchBoardHandler(logger, tenantStore, publisher))
	authenticated.DELETE("/boards/:board_id", deleteBoardHandler(logger, tenantStore, publisher))

	authenticated.POST("/boards/:board_id/columns", createColumnHandler(logger, tenantStore, publisher))
	authenticated.GET("/boards/:board_id/columns", listColumnsHandler(logger, tenantStore))
	authenticated.PATCH("/columns/:column_id", patchColumnHandler(logger, tenantStore, publisher))
	authenticated.POST("/columns/:column_id/move", moveColumnHandler(logger, tenantStore, publisher))
	authenticated.DELETE("/columns/:column_id", deleteColumnHandler(logger, tenantStore, publisher))

	authenticated.POST("/columns/:column_id/cards", createCardHandler(logger, tenantStore, publisher))
	authenticated.GET("/columns/:column_id/cards", listCardsByColumnHandler(logger, tenantStore))
	authenticated.GET("/boards/:board_id/cards", listCardsByBoardHandler(logger, tenantStore))
	authenticated.GET("/cards/:card_id", getCardHandler(logger, tenantStore))
	authenticated.PATCH("/cards/:card_id", patchCardHandler(logger, tenantStore, publisher))

	// A move is a POST rather than a PATCH on the card: it is not a partial
	// update of the card's fields, it is an operation whose arguments (the
	// target column and the anchor) are not properties of the card at all.
	authenticated.POST("/cards/:card_id/move", moveCardHandler(logger, tenantStore, publisher))
	authenticated.DELETE("/cards/:card_id", deleteCardHandler(logger, tenantStore, publisher))
}
