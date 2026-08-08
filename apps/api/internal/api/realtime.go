package api

// Where the WebSocket routes are mounted, and how a browser presents its token
// on a handshake.
//
// # Why the handlers arrive as values
//
// internal/realtime imports this package, for PrincipalFromContext: the context
// key is unexported here, so that is the only way to read a principal and there
// is no way to plant one. Making the dependency run in that direction is a
// deliberate choice — the alternative, injecting a "get me the principal"
// function into the hub, would put the tenant-comes-from-the-token rule behind
// a value main supplies, and a test could supply a different one.
//
// The cost is that this package cannot import realtime, so the router receives
// the two handlers as gin.HandlerFunc values and keeps ownership of the paths
// and the middleware. cmd/api builds the hub and passes them in, the same way
// it already passes the auth service.

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// RealtimeDeps are the WebSocket handlers, supplied by cmd/api.
//
// Both are optional, so a build without realtime — the health-only
// configuration several tests use — still produces a working engine.
type RealtimeDeps struct {
	// Connect upgrades to a WebSocket. Mounted at GET /api/v1/ws.
	Connect gin.HandlerFunc

	// PublishEvent fans one event out to a board.
	// Mounted at POST /api/v1/boards/:board_id/events.
	PublishEvent gin.HandlerFunc
}

// bearerSubprotocolPrefix carries an access token on the WebSocket handshake.
//
// # Why this exists
//
// The browser WebSocket API cannot set request headers. That leaves three ways
// to present a bearer token on an upgrade: a query parameter, a cookie, or the
// Sec-WebSocket-Protocol header.
//
//   - A query parameter puts the token in the request line, which is what
//     access logs, proxy logs and Referer headers record. That is a credential
//     in a log file by construction.
//   - A cookie means an ambient credential, which means CSRF and cross-site
//     WebSocket hijacking become live concerns for a service that currently has
//     neither.
//   - Sec-WebSocket-Protocol is a header, is not logged by default, and is what
//     the Kubernetes API server does for exactly this reason.
//
// This is emphatically *not* a second token mechanism: the value is the same
// access token, verified by the same [requireAuth] with the same issuer,
// audience and signature checks. Only the transport of the credential differs,
// and only on the one route where the standard transport is unavailable.
//
// G101 pattern-matches the word "bearer". This is the *name* of a subprotocol,
// not a credential: the token it introduces arrives from the client.
//
//nolint:gosec // see above
const bearerSubprotocolPrefix = "bearer.collabboard.v1."

// websocketBearer lifts a subprotocol-borne access token into the Authorization
// header, for requireAuth to verify like any other.
//
// It runs before requireAuth and never after, it only fills a header that is
// absent, and it does no verification of its own — a token that arrives this
// way is exactly as trusted as one in an Authorization header, which is to say
// not at all until the issuer has checked its signature.
func websocketBearer() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") != "" {
			c.Next()

			return
		}

		for _, offered := range strings.Split(c.GetHeader("Sec-WebSocket-Protocol"), ",") {
			token, ok := strings.CutPrefix(strings.TrimSpace(offered), bearerSubprotocolPrefix)
			if !ok || token == "" {
				continue
			}

			c.Request.Header.Set("Authorization", "Bearer "+token)

			break
		}

		c.Next()
	}
}
