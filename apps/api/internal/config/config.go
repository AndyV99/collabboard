// Package config loads service configuration from the process environment.
//
// Every value has a default that matches the local docker-compose stack, so a
// fresh checkout runs with no environment set at all. Credentials are never
// hardcoded to anything but those local dev values, and production supplies all
// of them through the environment.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved configuration for the API service.
type Config struct {
	Env      string
	LogLevel string

	HTTP     HTTPConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	Auth     AuthConfig
	Realtime RealtimeConfig
}

// EnvDevelopment is the value of APP_ENV that relaxes the checks a deployed
// environment must not relax — chiefly the signing-secret requirement below.
const EnvDevelopment = "development"

// IsDevelopment reports whether this process is running the local loop.
func (c Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

// AuthConfig configures registration, login and sessions.
//
// The one value with no safe default is JWTSecret. Everything else here is a
// tuning knob whose wrong setting makes the service slow or annoying;
// JWTSecret's wrong setting makes every token forgeable, so Load refuses to
// start without it outside development. A committed default that "works" is how
// a placeholder reaches production.
type AuthConfig struct {
	// JWTSecret signs and verifies access tokens. At least 32 bytes.
	JWTSecret string

	// TokenIssuer and TokenAudience are stamped on issue and asserted on
	// verification, so a token minted by a different service sharing the secret
	// does not verify here.
	TokenIssuer   string
	TokenAudience string

	// AccessTokenTTL is the window in which a revoked session still works,
	// because nothing consults a datastore to validate an access token.
	AccessTokenTTL time.Duration

	// RefreshTokenTTL is how long a session can be kept alive by refreshing.
	RefreshTokenTTL time.Duration

	// ClockSkew is the leeway allowed on exp and nbf. It extends the life of
	// every expired token by exactly this much, so it is small.
	ClockSkew time.Duration

	// Argon2 cost parameters for *new* credentials. Existing credentials carry
	// their own, so raising these re-derives each account on its next login
	// rather than locking anyone out.
	Argon2MemoryKiB     int
	Argon2Iterations    int
	Argon2Parallelism   int
	Argon2KeyLength     int
	Argon2SaltLength    int
	Argon2MaxConcurrent int

	// Login rate limits. Two budgets sharing one window — see internal/auth.
	LoginRatePerAccount int
	LoginRatePerAddress int
	LoginRateWindow     time.Duration
}

// RealtimeConfig configures the WebSocket hub and its Redis fan-out.
//
// Every value here is a tuning knob except AllowedOrigins, which is a security
// boundary — see [Load] for how its default is chosen.
type RealtimeConfig struct {
	// AllowedOrigins is the list of Origin patterns accepted on a WebSocket
	// handshake. Empty means same-origin only.
	AllowedOrigins []string

	// SendBuffer is how many frames may queue for one connection before it is
	// dropped as too slow.
	SendBuffer int

	// PingInterval is how often an idle connection is pinged; PongTimeout is
	// how long the pong may take before the connection is reaped.
	PingInterval time.Duration
	PongTimeout  time.Duration

	// WriteTimeout bounds a single frame write to one client.
	WriteTimeout time.Duration

	// ReadLimit caps an inbound frame, in bytes.
	ReadLimit int

	// ReauthorizeInterval is how often a live connection's membership and
	// subscriptions are re-checked against the database. It is the bound on how
	// long a revoked membership keeps receiving events.
	ReauthorizeInterval time.Duration

	// MaxRoomsPerConnection caps how many boards one socket may watch.
	MaxRoomsPerConnection int

	// BrokerBuffer is how many inbound pub/sub messages may queue.
	BrokerBuffer int

	// ShutdownReconnectHint is the base delay a draining instance asks clients
	// to wait before reconnecting. Jittered per connection.
	ShutdownReconnectHint time.Duration
}

// HTTPConfig configures the public HTTP listener.
type HTTPConfig struct {
	Host string
	Port int

	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration

	// MaxRequestBytes caps a request body on every route. net/http bounds
	// headers (MaxHeaderBytes) and nothing else, so without this a single POST
	// can make the process allocate whatever the caller cares to send.
	//
	// The default is 256 KiB, chosen against the largest body the field limits
	// in internal/api permit rather than picked round: a card carries a title of
	// at most 200 runes and a description of at most 10 000, those limits are
	// counted in runes *after* decoding, and one rune costs at most 12 bytes of
	// JSON (an astral character written as an escaped surrogate pair, which is
	// twelve ASCII bytes for one rune). 10 200 runes is therefore ~120 KiB on
	// the wire in the worst case, and 256 KiB is twice that.
	MaxRequestBytes int

	// MaxUnauthenticatedRequestBytes caps a request body on the routes that
	// answer before anyone has proved who they are — /auth/register, /login,
	// /refresh, /logout.
	//
	// Tighter than MaxRequestBytes on purpose. Those four are the endpoints an
	// anonymous caller can reach, so they are where an oversized body is most
	// attractive, and none of them carries anything large: an email is at most
	// 254 bytes, a password 128, a display name 128 runes, a refresh token is
	// server-generated and short. 16 KiB is several times the worst case even
	// with every field escaped character-by-character, and it cuts what an
	// unauthenticated caller can make this process read by 16x.
	MaxUnauthenticatedRequestBytes int
}

// Addr renders the listen address for net/http.
func (h HTTPConfig) Addr() string {
	return fmt.Sprintf("%s:%d", h.Host, h.Port)
}

// PostgresConfig configures the pgx connection pool.
//
// Two identities, not one. User/Password is the serving role — collabboard_app,
// which owns nothing and is subject to row-level security. MigrationUser /
// MigrationPassword is collabboard_owner, which owns the schema and is the only
// one able to run DDL. Keeping them apart in config is what stops the API from
// accidentally connecting as the owner, which would defeat the isolation model
// (docs/adr/0001-tenant-isolation.md).
//
// Neither is the cluster's bootstrap superuser or the RDS master. The owner is
// a dedicated non-superuser role created by
// apps/api/scripts/provision/bootstrap-owner.sql, so that FORCE ROW LEVEL
// SECURITY is exercised by the role that owns the tables rather than silently
// skipped — see docs/adr/0006-database-role-provisioning.md.
//
// Password is also what `api provision` writes into the database, so there is
// one secret for the serving role rather than two values that have to agree.
type PostgresConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string

	MigrationUser     string
	MigrationPassword string

	MaxConns int32
}

// DSN renders the connection URL used by the API's pgx pool, as the serving
// role.
func (p PostgresConfig) DSN() string {
	return p.dsn(p.User, p.Password)
}

// MigrationDSN renders the connection URL used by `api migrate`, as the role
// that owns the schema.
func (p PostgresConfig) MigrationDSN() string {
	return p.dsn(p.MigrationUser, p.MigrationPassword)
}

// dsn renders a libpq-style connection URL. The password is URL-escaped rather
// than interpolated so that special characters in a real secret cannot corrupt
// the DSN.
func (p PostgresConfig) dsn(user, password string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   fmt.Sprintf("%s:%d", p.Host, p.Port),
		Path:   "/" + p.Database,
	}

	q := u.Query()
	q.Set("sslmode", p.SSLMode)
	u.RawQuery = q.Encode()

	return u.String()
}

// RedisConfig configures the go-redis client.
type RedisConfig struct {
	Host     string
	Port     int
	Password string
	DB       int
}

// Addr renders the host:port go-redis expects.
func (r RedisConfig) Addr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

// Load reads configuration from the environment, applying local-dev defaults
// for anything unset. It returns an error rather than exiting so that main
// controls the failure path.
func Load() (Config, error) {
	var errs []string

	cfg := Config{
		Env:      envString("APP_ENV", "development"),
		LogLevel: envString("LOG_LEVEL", "info"),
		HTTP: HTTPConfig{
			Host:              envString("HTTP_HOST", ""),
			Port:              envInt("HTTP_PORT", 8080, &errs),
			ReadHeaderTimeout: envDuration("HTTP_READ_HEADER_TIMEOUT", 10*time.Second, &errs),
			ShutdownTimeout:   envDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second, &errs),

			MaxRequestBytes: envInt("HTTP_MAX_REQUEST_BYTES", DefaultMaxRequestBytes, &errs),
			MaxUnauthenticatedRequestBytes: envInt(
				"HTTP_MAX_UNAUTHENTICATED_REQUEST_BYTES", DefaultMaxUnauthenticatedRequestBytes, &errs),
		},
		Postgres: PostgresConfig{
			Host:              envString("POSTGRES_HOST", "localhost"),
			Port:              envInt("POSTGRES_PORT", 5432, &errs),
			User:              envString("POSTGRES_USER", "collabboard_app"),
			Password:          envString("POSTGRES_PASSWORD", "dev"),
			Database:          envString("POSTGRES_DB", "collabboard"),
			SSLMode:           envString("POSTGRES_SSLMODE", "disable"),
			MigrationUser:     envString("POSTGRES_MIGRATION_USER", "collabboard_owner"),
			MigrationPassword: envString("POSTGRES_MIGRATION_PASSWORD", "dev"),
			MaxConns:          int32(envInt("POSTGRES_MAX_CONNS", 10, &errs)), //nolint:gosec // bounded small int from config
		},
		Redis: RedisConfig{
			Host:     envString("REDIS_HOST", "localhost"),
			Port:     envInt("REDIS_PORT", 6379, &errs),
			Password: envString("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0, &errs),
		},
		Auth: AuthConfig{
			JWTSecret:     envString("AUTH_JWT_SECRET", ""),
			TokenIssuer:   envString("AUTH_TOKEN_ISSUER", "collabboard"),
			TokenAudience: envString("AUTH_TOKEN_AUDIENCE", "collabboard-api"),

			AccessTokenTTL:  envDuration("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute, &errs),
			RefreshTokenTTL: envDuration("AUTH_REFRESH_TOKEN_TTL", 14*24*time.Hour, &errs),
			ClockSkew:       envDuration("AUTH_CLOCK_SKEW", 30*time.Second, &errs),

			Argon2MemoryKiB:     envInt("AUTH_ARGON2_MEMORY_KIB", 19456, &errs),
			Argon2Iterations:    envInt("AUTH_ARGON2_ITERATIONS", 2, &errs),
			Argon2Parallelism:   envInt("AUTH_ARGON2_PARALLELISM", 1, &errs),
			Argon2KeyLength:     envInt("AUTH_ARGON2_KEY_LENGTH", 32, &errs),
			Argon2SaltLength:    envInt("AUTH_ARGON2_SALT_LENGTH", 16, &errs),
			Argon2MaxConcurrent: envInt("AUTH_ARGON2_MAX_CONCURRENT", 4, &errs),

			LoginRatePerAccount: envInt("AUTH_LOGIN_RATE_PER_ACCOUNT", 5, &errs),
			LoginRatePerAddress: envInt("AUTH_LOGIN_RATE_PER_ADDRESS", 30, &errs),
			LoginRateWindow:     envDuration("AUTH_LOGIN_RATE_WINDOW", 15*time.Minute, &errs),
		},
		Realtime: RealtimeConfig{
			SendBuffer:            envInt("REALTIME_SEND_BUFFER", 64, &errs),
			PingInterval:          envDuration("REALTIME_PING_INTERVAL", 25*time.Second, &errs),
			PongTimeout:           envDuration("REALTIME_PONG_TIMEOUT", 10*time.Second, &errs),
			WriteTimeout:          envDuration("REALTIME_WRITE_TIMEOUT", 5*time.Second, &errs),
			ReadLimit:             envInt("REALTIME_READ_LIMIT_BYTES", 32<<10, &errs),
			ReauthorizeInterval:   envDuration("REALTIME_REAUTHORIZE_INTERVAL", 30*time.Second, &errs),
			MaxRoomsPerConnection: envInt("REALTIME_MAX_BOARDS_PER_CONNECTION", 16, &errs),
			BrokerBuffer:          envInt("REALTIME_BROKER_BUFFER", 256, &errs),
			ShutdownReconnectHint: envDuration("REALTIME_SHUTDOWN_RECONNECT_HINT", time.Second, &errs),
		},
	}

	cfg.Auth = resolveAuthSecret(cfg.Env, cfg.Auth, &errs)
	cfg.Realtime.AllowedOrigins = resolveAllowedOrigins(cfg.Env)

	checkBodyLimits(cfg.HTTP, &errs)

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}

	return cfg, nil
}

// The request body limits, in bytes. See [HTTPConfig] for how each number was
// arrived at.
const (
	DefaultMaxRequestBytes                = 256 << 10
	DefaultMaxUnauthenticatedRequestBytes = 16 << 10
)

// checkBodyLimits refuses a configuration that would switch the limits off.
//
// Zero or negative is the setting that matters: an operator reaching for
// "unlimited" would write it, and internal/api would then have to decide what it
// meant. It means nothing here — the service does not start — so there is no
// environment in which the limit is silently absent. The other check is that the
// unauthenticated limit is the tighter of the two, because the global one is
// applied first and a looser value below it would never take effect: a limit
// that is configured and does nothing is worse than one that is not configured.
func checkBodyLimits(cfg HTTPConfig, errs *[]string) {
	if cfg.MaxRequestBytes <= 0 {
		*errs = append(*errs, fmt.Sprintf(
			"HTTP_MAX_REQUEST_BYTES is %d; it must be a positive number of bytes", cfg.MaxRequestBytes))
	}

	if cfg.MaxUnauthenticatedRequestBytes <= 0 {
		*errs = append(*errs, fmt.Sprintf(
			"HTTP_MAX_UNAUTHENTICATED_REQUEST_BYTES is %d; it must be a positive number of bytes",
			cfg.MaxUnauthenticatedRequestBytes))

		return
	}

	if cfg.MaxRequestBytes > 0 && cfg.MaxUnauthenticatedRequestBytes > cfg.MaxRequestBytes {
		*errs = append(*errs, fmt.Sprintf(
			"HTTP_MAX_UNAUTHENTICATED_REQUEST_BYTES (%d) exceeds HTTP_MAX_REQUEST_BYTES (%d), so it would never apply",
			cfg.MaxUnauthenticatedRequestBytes, cfg.MaxRequestBytes))
	}
}

// minJWTSecretLength mirrors internal/auth's floor. Duplicated rather than
// imported so that config does not depend on auth — the check has to happen
// here, at load, so an operator gets a message naming the environment variable
// instead of a wiring error three constructors later.
const minJWTSecretLength = 32

// resolveAuthSecret enforces the one setting with no safe default.
//
// Outside development the secret is required and must be long enough for HS256
// to mean anything: the signature's strength is capped by the key's entropy, so
// a short secret is a token forgery away from being brute-forced offline by
// anyone holding one valid token.
//
// In development a random secret is generated per process rather than defaulted
// to a constant. A constant in a committed file is a real credential the moment
// someone copies the compose stack to a shared host, and it would silently
// verify tokens minted by anyone who has read this repository. The cost of
// randomising is that a local restart invalidates local tokens, which is the
// correct amount of inconvenience.
func resolveAuthSecret(env string, auth AuthConfig, errs *[]string) AuthConfig {
	if auth.JWTSecret != "" {
		if len(auth.JWTSecret) < minJWTSecretLength {
			*errs = append(*errs, fmt.Sprintf(
				"AUTH_JWT_SECRET is %d bytes, minimum %d", len(auth.JWTSecret), minJWTSecretLength))
		}

		return auth
	}

	if env != EnvDevelopment {
		*errs = append(*errs, "AUTH_JWT_SECRET is required outside development")

		return auth
	}

	buf := make([]byte, minJWTSecretLength)
	if _, err := rand.Read(buf); err != nil {
		*errs = append(*errs, "generating a development AUTH_JWT_SECRET: "+err.Error())

		return auth
	}

	auth.JWTSecret = hex.EncodeToString(buf)

	return auth
}

// resolveAllowedOrigins decides which Origins may open a WebSocket.
//
// Unset means **same-origin only**, which is the strict answer and the wrong
// one for this product: the SPA is served from another host, so a deployed
// environment has to name it. Defaulting to "*" so that it works out of the box
// is the shortcut this function exists to refuse — an origin allow-list that
// defaults to open is an allow-list nobody ever configures.
//
// Development gets the local Next.js origins, for the same reason the JWT
// secret gets a random one there: the local loop should work from a fresh
// checkout, and a value that only applies when APP_ENV=development cannot reach
// production by being forgotten.
//
// Note that this is defence in depth rather than the primary control. The
// credential on this path is a bearer token the browser does not attach
// ambiently, so cross-site WebSocket hijacking does not apply today; the
// allow-list is what keeps that true if a cookie is ever added.
func resolveAllowedOrigins(env string) []string {
	if raw, ok := os.LookupEnv("REALTIME_ALLOWED_ORIGINS"); ok && strings.TrimSpace(raw) != "" {
		var origins []string

		for _, origin := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(origin); trimmed != "" {
				origins = append(origins, trimmed)
			}
		}

		return origins
	}

	if env != EnvDevelopment {
		return nil
	}

	return []string{"localhost:3000", "127.0.0.1:3000"}
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}

	return def
}

func envInt(key string, def int, errs *[]string) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}

	v, err := strconv.Atoi(raw)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not an integer", key, raw))

		return def
	}

	return v
}

func envDuration(key string, def time.Duration, errs *[]string) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}

	v, err := time.ParseDuration(raw)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s=%q is not a duration", key, raw))

		return def
	}

	return v
}
