// Command api is the entrypoint for the CollabBoard HTTP API.
//
// It does wiring only — load config, open the Postgres pool and Redis client,
// build the router with those dependencies injected, serve, and shut down
// cleanly. All behaviour lives under internal/.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
	"github.com/AndyV99/collabboard/apps/api/internal/config"
	"github.com/AndyV99/collabboard/apps/api/internal/logging"
)

const (
	serviceName = "collabboard-api"

	// startupPingTimeout bounds the optional connectivity probe at boot. It is
	// advisory only — a failure is logged, not fatal.
	startupPingTimeout = 3 * time.Second
)

func main() {
	// Installed before anything can fail so that even a config error is emitted
	// as JSON rather than through slog's default text handler.
	slog.SetDefault(logging.New(os.Stdout, serviceName, "info"))

	if err := run(); err != nil {
		slog.Error("api exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, serviceName, cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := newPostgresPool(ctx, logger, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisClient := newRedisClient(cfg.Redis)
	defer func() {
		if cerr := redisClient.Close(); cerr != nil {
			logger.Error("closing redis client", slog.Any("error", cerr))
		}
	}()

	if cfg.Env != "development" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := api.NewRouter(logger, api.HealthDeps{
		Postgres: pgxPinger{pool: pool},
		Redis:    redisPinger{client: redisClient},
	})

	server := &http.Server{
		Addr:              cfg.HTTP.Addr(),
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
	}

	return serve(ctx, logger, server, cfg.HTTP.ShutdownTimeout)
}

// serve runs the HTTP server until the context is cancelled, then drains it.
func serve(ctx context.Context, logger *slog.Logger, server *http.Server, shutdownTimeout time.Duration) error {
	errCh := make(chan error, 1)

	go func() {
		logger.Info("http server listening", slog.String("addr", server.Addr))

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}

		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	// Deliberately detached from ctx: ctx is already cancelled by the signal, so
	// inheriting it would abort the drain immediately instead of giving
	// in-flight requests shutdownTimeout to finish.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	logger.Info("http server stopped")

	return nil
}

// newPostgresPool builds the pgx pool. pgxpool connects lazily, so an
// unreachable database does not stop the process from starting — that is
// deliberate: the service comes up and reports itself unhealthy via /healthz
// instead of crash-looping before it can explain why.
func newPostgresPool(ctx context.Context, logger *slog.Logger, cfg config.PostgresConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parsing postgres config: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("creating postgres pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, startupPingTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		logger.Warn("postgres unreachable at startup; /healthz will report unhealthy",
			slog.String("host", cfg.Host),
			slog.Int("port", cfg.Port),
			slog.Any("error", err),
		)
	}

	return pool, nil
}

func newRedisClient(cfg config.RedisConfig) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     cfg.Addr(),
		Password: cfg.Password,
		DB:       cfg.DB,
	})
}

// pgxPinger and redisPinger adapt the driver clients to api.Pinger so that
// internal/api depends on an interface rather than on pgx and go-redis.
type pgxPinger struct{ pool *pgxpool.Pool }

func (p pgxPinger) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

type redisPinger struct{ client *redis.Client }

func (r redisPinger) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }
