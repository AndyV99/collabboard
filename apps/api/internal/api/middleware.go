package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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

		c.Next()

		status := c.Writer.Status()

		level := slog.LevelInfo
		if status >= http.StatusInternalServerError {
			level = slog.LevelError
		}

		logger.LogAttrs(c.Request.Context(), level, "http request",
			slog.String("request_id", requestID),
			slog.String("method", c.Request.Method),
			slog.String("path", c.FullPath()),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
		)
	}
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
