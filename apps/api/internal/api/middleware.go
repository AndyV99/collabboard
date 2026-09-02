package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/logging"
)

// requestIDHeader is echoed back on every response so a client-side error can
// be correlated with a server-side log line.
const requestIDHeader = "X-Request-ID"

// requestLogger emits one structured log line per request, using the field
// names the Observability standard asks for. It trusts an inbound request ID if
// present so that a trace survives a hop from the web app.
func requestLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		requestID := c.GetHeader(requestIDHeader)
		if requestID == "" {
			requestID = uuid.NewString()
		}

		c.Set(requestIDHeader, requestID)
		c.Header(requestIDHeader, requestID)

		// On the *request's* context, not only in gin's, so that every package
		// below this one can reach it without importing gin. internal/auth and
		// internal/realtime both log during a request and neither has a
		// *gin.Context; this is what lets their lines join to this one.
		//
		// Replacing c.Request is how a gin middleware amends the context --
		// every handler downstream reads c.Request.Context().
		c.Request = c.Request.WithContext(
			logging.WithRequestID(c.Request.Context(), requestID))

		c.Next()

		status := c.Writer.Status()

		level := slog.LevelInfo
		if status >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		// No explicit request_id: logging.ContextHandler adds it from the
		// context set above. Passing it here too would put the field on the
		// line twice, which in JSON is a duplicate key -- valid, and read
		// differently by different consumers.
		logger.LogAttrs(c.Request.Context(), level, "http request",
			slog.String("method", c.Request.Method),
			slog.String("path", c.FullPath()),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
		)
	}
}

// Request body limits.
//
// # Why this is middleware and not a check in a handler
//
// net/http bounds request *headers* (MaxHeaderBytes) and nothing else. A body is
// whatever the caller sends, and [bindJSON] hands it straight to
// encoding/json — so before this existed, a POST to /api/v1/auth/login with a
// multi-gigabyte body was read into memory by a process that had not yet decided
// whether the caller was anybody at all. The field limits in crud.go and
// internal/auth bound what reaches the database; they run after the body has
// already been read, which is one refusal too late.
//
// Being middleware is the whole point: it runs before [requireAuth], so the
// limit applies to the unauthenticated surface, and before any handler, so no
// handler can forget it.
//
// # Two limits
//
// The tighter one is applied to the five routes an anonymous caller can reach:
// the four under /auth, and POST /organizations, which takes a password for the
// reason internal/auth/organizations.go explains. The arithmetic behind both
// numbers is on HTTPConfig in internal/config.
//
// # What a refusal costs the connection
//
// Nothing is drained here. When the body is refused unread, net/http discards up
// to 256 KiB of the remainder looking for the end of the request, and closes the
// connection with `Connection: close` if more than that is left rather than
// trying to parse the leftover bytes as the next request. So an oversized body
// costs one bounded read and one closed connection, never a partially consumed
// one — and never the body itself.
const (
	// fallbackMaxRequestBytes and fallbackMaxUnauthenticatedRequestBytes apply
	// when a caller leaves [BodyLimits] zero, which in this repository means a
	// test that is not about body size.
	//
	// They exist so that "unset" is a working limit rather than no limit: a
	// router built without one would be a router that reads whatever it is sent,
	// and that is exactly the state this file was written to end. They are the
	// same numbers as config.DefaultMaxRequestBytes and
	// config.DefaultMaxUnauthenticatedRequestBytes, duplicated because
	// internal/api does not import internal/config —
	// TestTheFallbackLimitsMatchTheConfiguredDefaults keeps them honest.
	fallbackMaxRequestBytes                = 256 << 10
	fallbackMaxUnauthenticatedRequestBytes = 16 << 10
)

// messageBodyTooLarge is the body of every 413. It describes the request, not
// the limit: naming the number would only tell a caller how much it may send
// before being refused, and every legitimate client is orders of magnitude under
// it.
const messageBodyTooLarge = "request body is too large"

// BodyLimits is how much of a request body this service will read, in bytes.
//
// Zero means "use the built-in default", not "unlimited" — see
// [fallbackMaxRequestBytes]. cmd/api fills both fields from configuration.
type BodyLimits struct {
	// Default applies to every route.
	Default int64

	// Unauthenticated applies to the routes that answer before the caller has
	// proved who they are. It should be the smaller of the two.
	Unauthenticated int64
}

// resolved returns the limits actually enforced, substituting the defaults for
// anything unset. A non-positive value is treated as unset rather than as
// "unlimited": there is no configuration of this service in which a body is
// unbounded.
func (l BodyLimits) resolved() BodyLimits {
	if l.Default <= 0 {
		l.Default = fallbackMaxRequestBytes
	}

	if l.Unauthenticated <= 0 || l.Unauthenticated > l.Default {
		l.Unauthenticated = min(fallbackMaxUnauthenticatedRequestBytes, l.Default)
	}

	return l
}

// limitRequestBody refuses a body over limit bytes with 413, and bounds every
// body that is not obviously over it.
//
// Both halves are necessary. Content-Length is a claim: it can be absent
// entirely (a chunked request declares no length) and it can lie, so a check on
// the header alone is not a limit. [http.MaxBytesReader] is the actual bound —
// it counts what is read and fails the read past the limit, whatever the header
// said. The header check in front of it is what turns an honestly-declared
// oversized request into a refusal that reads no body at all.
//
// The reader's error surfaces as [http.MaxBytesError] out of ShouldBindJSON, and
// [bindJSON] maps it to the same 413 this writes, so a caller cannot tell the two
// paths apart.
func limitRequestBody(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > limit {
			abortBodyTooLarge(c)

			return
		}

		// Defensive: a server request always has a body, but a hand-built
		// *http.Request in a test need not, and MaxBytesReader would panic on
		// the first read of a nil one.
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}

		c.Next()
	}
}

// abortBodyTooLarge writes the one 413 this service produces.
//
// Nothing is logged. The refusal is already one line in the request log with its
// status, and the only detail a second line could add is the size or the content
// of a body this service deliberately refused to read.
func abortBodyTooLarge(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, errorResponse{Error: messageBodyTooLarge})
}

// recovery turns a panic in a handler into a logged 500 rather than a dead
// process. gin.Recovery would do this too, but it writes an unstructured stack
// trace to stdout.
func recovery(logger *slog.Logger) gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, recovered any) {
		logger.ErrorContext(c.Request.Context(), "panic recovered",
			slog.Any("panic", recovered),
			slog.String("path", c.FullPath()),
		)

		c.AbortWithStatusJSON(http.StatusInternalServerError, errorResponse{Error: messageInternalError})
	})
}
