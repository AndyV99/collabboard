package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// checkTimeout bounds each dependency probe so that an unreachable dependency
// fails the health check quickly instead of holding the request open.
const checkTimeout = 2 * time.Second

// Pinger is the minimal behaviour /healthz needs from a backing dependency.
// Both the pgx pool and the go-redis client are adapted to it in cmd/api, which
// keeps this package free of driver types and makes the handler testable
// without real infrastructure.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthDeps are the dependencies /healthz reports on. They are injected rather
// than reached for through package-level state.
type HealthDeps struct {
	Postgres Pinger
	Redis    Pinger
}

type componentStatus struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type healthResponse struct {
	Status     string                     `json:"status"`
	Components map[string]componentStatus `json:"components"`
}

const (
	statusOK      = "ok"
	statusDegrade = "unavailable"

	componentPostgres = "postgres"
	componentRedis    = "redis"
)

// healthHandler reports liveness of the process together with reachability of
// Postgres and Redis. It returns 503 if either dependency is unreachable, and
// never panics on a nil or failing dependency.
func healthHandler(logger *slog.Logger, deps HealthDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		components := probeAll(c.Request.Context(), map[string]Pinger{
			componentPostgres: deps.Postgres,
			componentRedis:    deps.Redis,
		})

		status := http.StatusOK
		body := healthResponse{Status: statusOK, Components: components}

		for name, component := range components {
			if component.Status == statusOK {
				continue
			}

			status = http.StatusServiceUnavailable
			body.Status = statusDegrade

			logger.WarnContext(c.Request.Context(), "health check dependency unavailable",
				slog.String("component", name),
				slog.String("error", component.Error),
			)
		}

		c.JSON(status, body)
	}
}

// probeAll pings every dependency concurrently, so the health check costs
// roughly one checkTimeout regardless of how many dependencies are added later
// rather than the sum of them.
func probeAll(ctx context.Context, pingers map[string]Pinger) map[string]componentStatus {
	var (
		mu         sync.Mutex
		wg         sync.WaitGroup
		components = make(map[string]componentStatus, len(pingers))
	)

	for name, p := range pingers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			status := probe(ctx, p)

			mu.Lock()
			defer mu.Unlock()

			components[name] = status
		}()
	}

	wg.Wait()

	return components
}

func probe(ctx context.Context, p Pinger) componentStatus {
	if p == nil {
		return componentStatus{Status: statusDegrade, Error: "dependency not configured"}
	}

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	if err := p.Ping(ctx); err != nil {
		return componentStatus{Status: statusDegrade, Error: err.Error()}
	}

	return componentStatus{Status: statusOK}
}
