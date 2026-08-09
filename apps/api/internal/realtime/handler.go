package realtime

// The WebSocket upgrade handler.
//
// It lives here rather than in internal/api so that the principal can be read
// with api.PrincipalFromContext directly, from the package that owns the
// context key. internal/api receives it as a value (see its RealtimeDeps) and
// decides the path and the middleware; the import therefore runs
// realtime -> api and never the other way, which is what keeps that key
// unexported and unforgeable.
//
// There used to be a second handler here — POST /api/v1/boards/:board_id/events
// — which existed only so that issue #9's fan-out could be demonstrated before
// anything could move a card. Issue #45 replaced it with the real thing: the
// card and column write paths publish after their transactions commit. See
// publisher.go and internal/api/events.go.

import (
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
)

// Messages this package puts in an HTTP error body or a client error frame.
// Neither describes stored state.
//
// The set shrank with the publish endpoint: an HTTP response from this package
// is now only "you are not authenticated" or "this instance is draining", and
// everything a client is told about a board it may not watch travels as a frame
// reason instead.
const (
	messageBoardIDNotUUID  = "board_id must be a uuid"
	messageUnauthenticated = "authentication required"
)

// errorBody is the error shape this handler returns. It matches
// internal/api's errorResponse deliberately — a client should not be able to
// tell which package answered it — and is declared here because internal/api
// cannot be imported for its unexported type.
type errorBody struct {
	Error string `json:"error"`
}

// ConnectHandler upgrades an authenticated request to a WebSocket and serves it
// until it ends.
//
// It must be mounted behind the same requireAuth middleware as the REST routes.
// There is no fallback: a request without a principal in its context is
// refused, so mounting this outside the authenticated tree fails closed rather
// than serving anonymously.
//
// The handler blocks for the connection's lifetime. That is correct for a
// hijacked connection — the HTTP goroutine has nothing else to do — but it does
// mean http.Server.Shutdown will not touch it, which is why [Hub.Shutdown]
// exists and why cmd/api calls it first.
func (h *Hub) ConnectHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := api.PrincipalFromContext(c.Request.Context())
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, errorBody{Error: messageUnauthenticated})

			return
		}

		h.mu.RLock()
		closing := h.closing
		h.mu.RUnlock()

		if closing {
			// A draining instance refuses rather than accepts-and-immediately-
			// closes, so a client behind a load balancer retries onto a healthy
			// instance instead of reconnecting to this one.
			c.Header("Retry-After", "1")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, errorBody{Error: "instance is restarting"})

			return
		}

		ws, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			Subprotocols: []string{Subprotocol},

			// Empty means same-origin only, which is coder/websocket's default
			// and the right one for a same-origin deployment. The SPA is on
			// another origin, so this is configured — see internal/config.
			// Bearer tokens already make cross-site WebSocket hijacking a
			// non-issue here (no ambient credential is attached by the
			// browser); the check is defence in depth against the day someone
			// adds a cookie.
			OriginPatterns: h.cfg.AllowedOrigins,
		})
		if err != nil {
			// Accept has already written the response.
			h.logger.Info("realtime upgrade refused",
				slog.String("event", "realtime.upgrade.refused"),
				slog.String("client_ip", c.ClientIP()),
				slog.Any("error", err),
			)

			return
		}

		newConn(h, ws, principal).serve(c.Request.Context())
	}
}
