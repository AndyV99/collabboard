//go:build integration

package redistest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// Image is pinned to the same major version as docker-compose.yml, so the
// suite exercises the server the local stack runs.
const Image = "redis:7-alpine"

// startTimeout bounds pull, start and wait-for-ready.
const startTimeout = 3 * time.Minute

// pingTimeout bounds the "is this client usable" probe.
const pingTimeout = 10 * time.Second

// Server is a running Redis container.
type Server struct {
	container *tcredis.RedisContainer

	// URL is the connection string, in redis:// form.
	URL string
}

// Start brings up Redis on a random host port.
//
// The caller owns the returned Server and must call Close. On any failure
// during bring-up the container is terminated before the error is returned, so
// a half-started harness does not leak one.
func Start(ctx context.Context) (*Server, error) {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	container, err := tcredis.Run(ctx, Image)
	if err != nil {
		// Run can return a non-nil container alongside an error, so termination
		// is attempted regardless.
		return nil, errors.Join(
			fmt.Errorf("starting redis container: %w", err),
			testcontainers.TerminateContainer(container),
		)
	}

	server := &Server{container: container}

	url, err := container.ConnectionString(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("reading redis connection string: %w", err), server.Close())
	}

	server.URL = url

	return server, nil
}

// Close terminates the container. Safe on a nil or partially built Server.
//
// Best-effort by design, for the same reason pgtest's is: a panic on a test
// goroutine kills the process without unwinding, and Testcontainers' Ryuk
// reaper is the backstop that removes the container anyway.
func (s *Server) Close() error {
	if s == nil || s.container == nil {
		return nil
	}

	if err := testcontainers.TerminateContainer(s.container); err != nil {
		return fmt.Errorf("terminating redis container: %w", err)
	}

	return nil
}

// Client returns a client for this server, closed when the test ends.
//
// Each call selects a fresh logical database and flushes it, so tests that share
// the container cannot see each other's keys. That matters here in a way it does
// not for Postgres: the auth keys are named after hashes and session ids rather
// than tenants, so there is no policy making one test's keys invisible to
// another's — only the database separation.
func (s *Server) Client(tb testing.TB, db int) *redis.Client {
	tb.Helper()

	if s == nil || s.URL == "" {
		tb.Fatal("redistest: harness was never started; TestMain did not run or failed")
	}

	// Deliberately not internal/redisclient, which is the one place the service
	// itself may build a Redis client. This harness dials a Testcontainers
	// instance that runs without TLS and is addressed by a generated URL, so it
	// has no config.RedisConfig to hand and nothing to gain from the TLS
	// decision. Routing it through redisclient would make the test harness the
	// second definition of how this service connects, which is what that
	// package exists to prevent.
	opts, err := redis.ParseURL(s.URL)
	if err != nil {
		tb.Fatalf("redistest: parsing url: %v", err)
	}

	opts.DB = db

	client := redis.NewClient(opts)

	tb.Cleanup(func() {
		if cerr := client.Close(); cerr != nil {
			tb.Errorf("redistest: closing client: %v", cerr)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	// Fatal, not Skip. The container is up by the time any test asks for a
	// client, so an unreachable server is a broken harness — and a harness that
	// skips when it breaks is worse than no harness.
	if err := client.Ping(ctx).Err(); err != nil {
		tb.Fatalf("redistest: pinging redis: %v", err)
	}

	if err := client.FlushDB(ctx).Err(); err != nil {
		tb.Fatalf("redistest: flushing database %d: %v", db, err)
	}

	return client
}
