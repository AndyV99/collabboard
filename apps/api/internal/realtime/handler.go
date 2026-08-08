package realtime

// The two HTTP handlers.
//
// Both live here rather than in internal/api so that the principal can be read
// with api.PrincipalFromContext directly, from the package that owns the
// context key. internal/api receives them as values (see its RealtimeDeps) and
// decides the paths and the middleware; the import therefore runs
// realtime -> api and never the other way, which is what keeps that key
// unexported and unforgeable.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
)

// maxEventPayloadBytes caps a published event's payload. Generous for a card
// move, and small enough that an authenticated client cannot make the fleet
// hold megabytes per event in every instance's buffers.
const maxEventPayloadBytes = 16 << 10

// Messages this package puts in an HTTP error body or a client error frame.
// None of them describes stored state.
const (
	messageBoardIDNotUUID  = "board_id must be a uuid"
	messageUnauthenticated = "authentication required"
	messageForbidden       = "not authorized for that board"
	messageInternalError   = "internal server error"
)

// errorBody is the error shape these two handlers return. It matches
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

// publishRequest is the body of the event endpoint.
type publishRequest struct {
	// Type names what happened. Required.
	Type string `json:"type" binding:"required"`

	// Payload is passed through to subscribers untouched.
	Payload map[string]any `json:"payload"`
}

// PublishHandler publishes one event to a board's subscribers on every
// instance.
//
// # Scope note
//
// This endpoint exists to demonstrate fan-out and for no other reason. Issue #9
// is the realtime layer; board and card CRUD are somebody else's issue, and an
// acceptance criterion that says "two clients on the same board see each
// other's card moves" needs *something* to move a card. It writes nothing: the
// event is a signal, not a mutation, and it is authorized by exactly the same
// check a subscription is.
//
// When card CRUD lands, the card handler should call [Hub.Publish] in the same
// transaction boundary that persists the move, and this endpoint should go.
// Flagged in the pull request rather than left to be discovered.
func (h *Hub) PublishHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal, ok := api.PrincipalFromContext(c.Request.Context())
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, errorBody{Error: messageUnauthenticated})

			return
		}

		boardID, err := uuid.Parse(c.Param("board_id"))
		if err != nil || boardID == uuid.Nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorBody{Error: messageBoardIDNotUUID})

			return
		}

		var req publishRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorBody{Error: "request body is not valid"})

			return
		}

		// The same authorizer the subscribe path uses, so "who may publish to a
		// board" and "who may watch one" cannot drift apart. A board in another
		// organization is a 403 here for the same reason it is a refusal there.
		if err := h.cfg.Authorizer.AuthorizeBoard(c.Request.Context(), principal, boardID); err != nil {
			h.writeAuthorizeError(c, boardID, err)

			return
		}

		payload, err := encodePayload(req.Payload)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorBody{Error: "payload is not valid"})

			return
		}

		if len(payload) > maxEventPayloadBytes {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, errorBody{Error: "payload is too large"})

			return
		}

		event := Event{
			ID:   uuid.New(),
			Type: req.Type,
			// From the principal, never from the body: an event that could name
			// its own actor would let any member forge another member's action
			// in every open browser on the board.
			ActorID:    principal.UserID,
			OccurredAt: h.cfg.now().UTC(),
			Payload:    payload,
		}

		// principal.TenantID. The room's tenant half is not reachable from the
		// request, here or on the subscribe path.
		room := principalRoom(principal, boardID)

		if err := h.Publish(c.Request.Context(), room, event); err != nil {
			h.logger.ErrorContext(c.Request.Context(), "publishing a realtime event failed",
				slog.String("event", "realtime.publish.failed"),
				slog.String("room", room.String()),
				slog.Any("error", err),
			)

			c.AbortWithStatusJSON(http.StatusInternalServerError, errorBody{Error: messageInternalError})

			return
		}

		c.JSON(http.StatusAccepted, gin.H{
			"event_id":    event.ID.String(),
			"board_id":    boardID.String(),
			"occurred_at": event.OccurredAt.Format(time.RFC3339Nano),
		})
	}
}

// encodePayload renders the client's payload once, at the edge, so the fan-out
// path carries bytes rather than a map it would have to encode per instance.
func encodePayload(payload map[string]any) (json.RawMessage, error) {
	if len(payload) == 0 {
		return nil, nil
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the event payload: %w", err)
	}

	return encoded, nil
}

func (h *Hub) writeAuthorizeError(c *gin.Context, boardID uuid.UUID, err error) {
	if errors.Is(err, ErrForbidden) {
		c.AbortWithStatusJSON(http.StatusForbidden, errorBody{Error: messageForbidden})

		return
	}

	h.logger.ErrorContext(c.Request.Context(), "authorizing a realtime publish failed",
		slog.String("event", "realtime.publish.authorize_failed"),
		slog.String("board_id", boardID.String()),
		slog.Any("error", err),
	)

	c.AbortWithStatusJSON(http.StatusInternalServerError, errorBody{Error: messageInternalError})
}
