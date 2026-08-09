// Command api is the entrypoint for the CollabBoard HTTP API.
//
// It does wiring only — load config, open the Postgres pool and Redis client,
// build the router with those dependencies injected, serve, and shut down
// cleanly. All behaviour lives under internal/.
//
// Three modes, one binary:
//
//	api                  serve HTTP (the default)
//	api migrate <cmd>    apply or roll back schema migrations, then exit
//	api provision        set the serving role's password from configuration
//
// One binary because it keeps the image and the schema it expects inseparable.
// Separate modes because they connect as different database roles — see
// internal/migrate — and because a deploy has to be able to run the first two
// as a pre-deploy task while the old version is still serving.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/hkdf"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/config"
	"github.com/AndyV99/collabboard/apps/api/internal/logging"
	"github.com/AndyV99/collabboard/apps/api/internal/migrate"
	"github.com/AndyV99/collabboard/apps/api/internal/provision"
	"github.com/AndyV99/collabboard/apps/api/internal/realtime"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

const (
	serviceName = "collabboard-api"

	// migrateCommand and provisionCommand are the argv[1] values that switch
	// the binary out of serve mode.
	migrateCommand   = "migrate"
	provisionCommand = "provision"

	// startupPingTimeout bounds the optional connectivity probe at boot. It is
	// advisory only — a failure is logged, not fatal.
	startupPingTimeout = 3 * time.Second
)

func main() {
	// Installed before anything can fail so that even a config error is emitted
	// as JSON rather than through slog's default text handler.
	slog.SetDefault(logging.New(os.Stdout, serviceName, "info"))

	if err := run(os.Args[1:]); err != nil {
		slog.Error("api exited with error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(os.Stdout, serviceName, cfg.LogLevel)
	slog.SetDefault(logger)

	if len(args) > 0 {
		switch args[0] {
		case migrateCommand:
			return runMigrate(logger, cfg, args[1:])
		case provisionCommand:
			return runProvision(logger, cfg, args[1:])
		}
	}

	return runServe(logger, cfg)
}

// runMigrate applies migrations and exits. It connects with the migration DSN,
// not the pool DSN, because the serving role cannot run DDL by design.
func runMigrate(logger *slog.Logger, cfg config.Config, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: %s %s <%v>", os.Args[0], migrateCommand, migrate.Commands())
	}

	// Signal-aware as well: a migration that runs long enough for a deploy to
	// be cancelled should stop at a statement boundary rather than be killed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return migrate.Run(ctx, logger, cfg.Postgres.MigrationDSN(), migrate.Command(args[0]))
}

// runProvision gives the serving role the password the serving DSN is built
// from, and exits.
//
// One configuration value, two uses: POSTGRES_PASSWORD is both what this writes
// into the database and what `api` authenticates with. Two variables would be
// two things to rotate and one more way for a deploy to end up with a role
// whose password nothing knows.
//
// It connects with the migration DSN because setting another role's password
// requires administrative rights over it, which only the schema owner has. The
// serving role deliberately cannot rotate its own credential.
func runProvision(logger *slog.Logger, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: %s %s (takes no arguments)", os.Args[0], provisionCommand)
	}

	credentials := []provision.Credential{
		{Role: cfg.Postgres.User, Password: cfg.Postgres.Password},
	}

	// The serving role and the migration role must be different identities —
	// that separation is the whole of ADR 0001's answer to the owner trap — and
	// this command is where getting it wrong would do real damage: it would
	// rotate the owner's own password out from under the deploy.
	if cfg.Postgres.User == cfg.Postgres.MigrationUser {
		return fmt.Errorf(
			"POSTGRES_USER and POSTGRES_MIGRATION_USER are both %q; the serving role must not be the schema owner (docs/adr/0001-tenant-isolation.md)",
			cfg.Postgres.User)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("provisioning database role credentials",
		slog.String("roles", provision.Describe(credentials)),
		slog.String("as", cfg.Postgres.MigrationUser))

	return provision.Roles(ctx, logger, cfg.Postgres.MigrationDSN(), credentials...)
}

// runServe builds the HTTP service and runs it until a shutdown signal arrives.
func runServe(logger *slog.Logger, cfg config.Config) error {
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

	if !cfg.IsDevelopment() {
		gin.SetMode(gin.ReleaseMode)
	}

	dataStore := store.New(pool)

	authDeps, err := newAuthDeps(logger, cfg.Auth, dataStore, redisClient)
	if err != nil {
		return err
	}

	hub, err := newHub(logger, cfg.Realtime, dataStore, redisClient)
	if err != nil {
		return err
	}

	router := api.NewRouter(logger, api.BodyLimits{
		Default:         int64(cfg.HTTP.MaxRequestBytes),
		Unauthenticated: int64(cfg.HTTP.MaxUnauthenticatedRequestBytes),
	}, api.HealthDeps{
		Postgres: pgxPinger{pool: pool},
		Redis:    redisPinger{client: redisClient},
	}, authDeps, api.RealtimeDeps{
		Connect: hub.ConnectHandler(),
		// The write path's broadcaster. Every card and column write publishes
		// through this after its transaction commits — see
		// internal/api/events.go and docs/adr/0005-realtime-event-delivery.md.
		Publisher: hub.EventPublisher(),
	})

	server := &http.Server{
		Addr:              cfg.HTTP.Addr(),
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
	}

	// The hub drains before the HTTP server does, and that order is
	// load-bearing rather than tidy. A WebSocket is a hijacked connection, and
	// http.Server.Shutdown neither closes nor waits for those — it would return
	// "done" with every socket still open, and the process would then exit and
	// reset them. Draining the hub first means every client gets a shutdown
	// frame with a jittered reconnect delay and a Going Away close, which is
	// the difference between a rolling deploy nobody notices and a synchronised
	// reconnect storm. See internal/realtime/README.md.
	drain := func(drainCtx context.Context) {
		if err := hub.Shutdown(drainCtx); err != nil {
			logger.Error("draining the realtime hub", slog.Any("error", err))
		}
	}

	return serve(ctx, logger, server, cfg.HTTP.ShutdownTimeout, drain)
}

// newHub builds the WebSocket hub. Wiring only: every decision it encodes lives
// in internal/realtime.
func newHub(
	logger *slog.Logger,
	cfg config.RealtimeConfig,
	dataStore *store.Store,
	redisClient *redis.Client,
) (*realtime.Hub, error) {
	broker, err := realtime.NewRedisBroker(realtime.RedisBrokerConfig{
		Client: redisClient,
		Logger: logger,
		Buffer: cfg.BrokerBuffer,
	})
	if err != nil {
		return nil, err
	}

	hub, err := realtime.NewHub(realtime.HubConfig{
		Broker:                broker,
		Authorizer:            realtime.NewStoreAuthorizer(dataStore),
		Logger:                logger,
		SendBuffer:            cfg.SendBuffer,
		PingInterval:          cfg.PingInterval,
		PongTimeout:           cfg.PongTimeout,
		WriteTimeout:          cfg.WriteTimeout,
		ReadLimit:             int64(cfg.ReadLimit),
		ReauthorizeInterval:   cfg.ReauthorizeInterval,
		MaxRoomsPerConnection: cfg.MaxRoomsPerConnection,
		AllowedOrigins:        cfg.AllowedOrigins,
		ShutdownReconnectHint: cfg.ShutdownReconnectHint,
	})
	if err != nil {
		return nil, errors.Join(err, broker.Close())
	}

	return hub, nil
}

// serve runs the HTTP server until the context is cancelled, then drains it.
//
// drain runs before the HTTP shutdown and shares its deadline.
func serve(
	ctx context.Context,
	logger *slog.Logger,
	server *http.Server,
	shutdownTimeout time.Duration,
	drain func(context.Context),
) error {
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

	if drain != nil {
		drain(shutdownCtx)
	}

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

// absentSaltLabel domain-separates the salt used for the derivation login
// performs when no account matches, from the signing key it is derived out of.
//
// One configured secret, two uses, and HKDF's info parameter is exactly the
// mechanism for that: the two outputs are independent, so learning one — say by
// observing that a stand-in derivation happened — reveals nothing about the
// other. A second environment variable would be a second thing to rotate and a
// second thing to forget.
const absentSaltLabel = "collabboard/auth/absent-account-salt/v1"

// newAuthDeps constructs the auth service and the token verifier the HTTP layer
// needs. Wiring only: every decision it encodes lives in internal/auth.
func newAuthDeps(
	logger *slog.Logger,
	cfg config.AuthConfig,
	dataStore *store.Store,
	redisClient *redis.Client,
) (api.AuthDeps, error) {
	secret := []byte(cfg.JWTSecret)

	issuer, err := auth.NewIssuer(auth.TokenConfig{
		Secret:    secret,
		Issuer:    cfg.TokenIssuer,
		Audience:  cfg.TokenAudience,
		AccessTTL: cfg.AccessTokenTTL,
		Leeway:    cfg.ClockSkew,
	})
	if err != nil {
		return api.AuthDeps{}, err
	}

	params := auth.Argon2Params{
		MemoryKiB:   uint32(cfg.Argon2MemoryKiB),  //nolint:gosec // bounded by Argon2Params.Validate below
		Iterations:  uint32(cfg.Argon2Iterations), //nolint:gosec // bounded by Argon2Params.Validate below
		Parallelism: uint8(cfg.Argon2Parallelism), //nolint:gosec // bounded by Argon2Params.Validate below
		KeyLength:   uint32(cfg.Argon2KeyLength),  //nolint:gosec // bounded by Argon2Params.Validate below
		SaltLength:  uint32(cfg.Argon2SaltLength), //nolint:gosec // bounded by Argon2Params.Validate below
	}

	absentSalt, err := deriveKey(secret, absentSaltLabel, int(params.SaltLength))
	if err != nil {
		return api.AuthDeps{}, err
	}

	limiterPepper, err := deriveKey(secret, "collabboard/auth/rate-limit-pepper/v1", sha256.Size)
	if err != nil {
		return api.AuthDeps{}, err
	}

	kv := auth.NewRedisKeyValue(redisClient)

	rateLimits := auth.RateLimitConfig{
		PerAccount: cfg.LoginRatePerAccount,
		PerAddress: cfg.LoginRatePerAddress,
		Window:     cfg.LoginRateWindow,
	}
	if err := rateLimits.Validate(); err != nil {
		return api.AuthDeps{}, err
	}

	service, err := auth.NewService(auth.ServiceDeps{
		Store:      dataStore,
		Deriver:    auth.NewArgon2Deriver(cfg.Argon2MaxConcurrent),
		Issuer:     issuer,
		Sessions:   auth.NewSessionStore(kv, cfg.RefreshTokenTTL),
		Limiter:    auth.NewLimiter(kv, rateLimits, limiterPepper, logger),
		Logger:     logger,
		Params:     params,
		AbsentSalt: absentSalt,
	})
	if err != nil {
		return api.AuthDeps{}, err
	}

	return api.AuthDeps{Service: service, Verifier: issuer, Store: dataStore}, nil
}

// deriveKey expands a secret into an independent key of the requested length.
func deriveKey(secret []byte, label string, length int) ([]byte, error) {
	out := make([]byte, length)

	if _, err := io.ReadFull(hkdf.New(sha256.New, secret, nil, []byte(label)), out); err != nil {
		return nil, fmt.Errorf("deriving %s: %w", label, err)
	}

	return out, nil
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
