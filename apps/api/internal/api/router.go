// Package api wires the HTTP surface of the service: the Gin engine, shared
// middleware, and the handlers. Dependencies arrive through NewRouter; nothing
// in this package holds package-level state.
package api

import (
	"log/slog"

	"github.com/gin-gonic/gin"
)

// NewRouter builds the Gin engine with the service's middleware and routes.
func NewRouter(logger *slog.Logger, deps HealthDeps) *gin.Engine {
	router := gin.New()

	// gin.Logger writes unstructured text to stdout, which would violate the
	// structured-logging standard; requestLogger replaces it.
	router.Use(requestLogger(logger), recovery(logger))

	router.GET("/healthz", healthHandler(logger, deps))

	return router
}
