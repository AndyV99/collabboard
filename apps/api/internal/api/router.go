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
//
// limits is configuration rather than a dependency, which is why it comes first
// and not as another Deps struct. Its zero value is a working pair of limits,
// not an absent one — see [BodyLimits].
// trustedProxies is the CIDR list whose X-Forwarded-For this service believes.
// Nil means trust nobody, which is correct when nothing sits in front of the
// service and wrong the moment a load balancer does — see the call to
// SetTrustedProxies below for why the wrong one is the quiet one.
func NewRouter(
	logger *slog.Logger,
	limits BodyLimits,
	trustedProxies []string,
	deps HealthDeps,
	authDeps AuthDeps,
	realtimeDeps RealtimeDeps,
) *gin.Engine {
	limits = limits.resolved()

	router := gin.New()

	// Who is allowed to tell this service who the client is.
	//
	// Gin trusts X-Forwarded-For from every peer by default, which makes
	// ClientIP() attacker-controlled and the per-address login budget trivially
	// bypassable — one header per attempt and every attempt looks like a new
	// client. Trusting nobody makes ClientIP() the peer address.
	//
	// Both settings are wrong in a different direction and only one of them is
	// loud. With an ALB in front and nobody trusted, every request appears to
	// come from the load balancer, so the per-address budget becomes one bucket
	// shared by every user and a single attacker locks out everybody. That is
	// worse than not having the limit, and nothing about it looks broken.
	//
	// So the list is configuration, defaulting to empty — the current, safe
	// behaviour for a service with nothing in front of it — and the deployed
	// task definition sets it to the load balancer's own subnets. Anything
	// unparseable, and 0.0.0.0/0 specifically, is refused by config.Load before
	// the engine is built, because the failure here is a log line beside a
	// server that is already serving.
	if err := router.SetTrustedProxies(trustedProxies); err != nil {
		// Unreachable in practice: config.Load has already parsed every entry
		// with net.ParseCIDR. Kept because "unreachable" is a claim about
		// today's callers, and a router that silently trusts the wrong set is
		// not a failure anyone would notice.
		logger.Error("configuring trusted proxies", slog.Any("error", err))
	}

	// X-Forwarded-For only. Gin's default also consults X-Real-IP, and that is
	// a hole once anything upstream is trusted: an ALB *appends* to
	// X-Forwarded-For, so a forged entry ends up to the left of the real client
	// address and Gin's right-to-left walk ignores it — but the ALB passes
	// X-Real-IP through untouched, and Gin falls back to it whenever
	// X-Forwarded-For is absent or malformed. A client that sends a malformed
	// X-Forwarded-For and a chosen X-Real-IP would pick its own identity.
	//
	// One header, the one the load balancer actually writes.
	router.RemoteIPHeaders = []string{"X-Forwarded-For"}

	// gin.Logger writes unstructured text to stdout, which would violate the
	// structured-logging standard; requestLogger replaces it.
	//
	// limitRequestBody comes last of the three and applies to everything after
	// it, including /healthz and the WebSocket upgrade. Last because the other
	// two have to wrap it: a body refused with 413 is still a request the log
	// should show, and recovery has to be above anything that can panic. It
	// cannot go any lower — a limit mounted on the authenticated group would
	// leave the five routes an anonymous caller can actually reach as the only
	// unbounded ones, which is the entire vulnerability.
	router.Use(requestLogger(logger), recovery(logger), limitRequestBody(limits.Default))

	router.GET("/healthz", healthHandler(logger, deps))

	if authDeps.Service == nil || authDeps.Verifier == nil {
		return router
	}

	v1 := router.Group("/api/v1")

	// Unauthenticated. Each one is a credential-presentation endpoint, which is
	// why the rate limiter lives inside internal/auth rather than as middleware
	// here: it has to key on the account being attempted, and only the service
	// knows how to normalise that.
	//
	// They are the only routes with a second, tighter body limit, for the same
	// reason they are the only ones with a rate limiter: they are what an
	// anonymous caller can reach. The tighter reader wraps the global one, so it
	// is the one that trips first.
	unauthenticated := v1.Group("", limitRequestBody(limits.Unauthenticated))
	unauthenticated.POST("/auth/register", registerHandler(logger, authDeps.Service))
	unauthenticated.POST("/auth/login", loginHandler(logger, authDeps.Service))
	unauthenticated.POST("/auth/refresh", refreshHandler(logger, authDeps.Service))
	unauthenticated.POST("/auth/logout", logoutHandler(logger, authDeps.Service))

	// The odd one out, and here rather than on the authenticated group for a
	// structural reason rather than a convenient one: the account it serves has
	// no organization, so it has no tenant, so it cannot hold a token this
	// service would issue or verify. It presents a password like the four above
	// and is rate limited like login. See createOrganizationHandler and
	// internal/auth/organizations.go — and note that requireAuth is untouched by
	// it, which was the constraint that decided the design.
	unauthenticated.POST("/organizations", createOrganizationHandler(logger, authDeps.Service))

	// Everything below requires a valid access token, and takes its tenant from
	// that token's org claim. There is no route parameter for an organization
	// anywhere in this tree — see auth_middleware.go for why that is a design
	// decision rather than an omission.
	authenticated := v1.Group("", requireAuth(logger, authDeps.Verifier))
	authenticated.GET("/me", meHandler(logger, authDeps.Service))
	authenticated.POST("/auth/organization", switchOrganizationHandler(logger, authDeps.Service))

	// Adding a member goes through the service rather than the store, because
	// half of it — resolving an address to an account that may live entirely
	// outside this tenant's visibility — travels the pre-tenant door, and
	// internal/api has no access to that door and should not acquire one. So it
	// is mounted here with the other service-backed routes rather than beside
	// GET /members below, which needs only a tenant-scoped querier.
	authenticated.POST("/members", addMemberHandler(logger, authDeps.Service))

	if authDeps.Store != nil {
		authenticated.GET("/members", membersHandler(logger, authDeps.Store))

		// A board surface with no publisher commits writes that nobody is told
		// about, and does it silently. That is the correct configuration for a
		// router built without realtime — several tests, and nothing else — and
		// a wiring bug anywhere it is not, so it says so once at startup rather
		// than being discovered as "the board stopped updating".
		if realtimeDeps.Publisher == nil {
			logger.Warn("board routes mounted without a realtime publisher; writes will not be broadcast",
				slog.String("event", "realtime.publisher.absent"),
			)
		}

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
