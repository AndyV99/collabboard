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

// HTTPConfig configures the public HTTP listener.
type HTTPConfig struct {
	Host string
	Port int

	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
}

// Addr renders the listen address for net/http.
func (h HTTPConfig) Addr() string {
	return fmt.Sprintf("%s:%d", h.Host, h.Port)
}

// PostgresConfig configures the pgx connection pool.
//
// Two identities, not one. User/Password is the serving role — collabboard_app,
// which owns nothing and is subject to row-level security. MigrationUser /
// MigrationPassword is the role that owns the schema and is the only one able
// to run DDL. Keeping them apart in config is what stops the API from
// accidentally connecting as the owner, which would defeat the isolation model
// (docs/adr/0001-tenant-isolation.md).
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
		},
		Postgres: PostgresConfig{
			Host:              envString("POSTGRES_HOST", "localhost"),
			Port:              envInt("POSTGRES_PORT", 5432, &errs),
			User:              envString("POSTGRES_USER", "collabboard_app"),
			Password:          envString("POSTGRES_PASSWORD", "dev"),
			Database:          envString("POSTGRES_DB", "collabboard"),
			SSLMode:           envString("POSTGRES_SSLMODE", "disable"),
			MigrationUser:     envString("POSTGRES_MIGRATION_USER", "collabboard"),
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
	}

	cfg.Auth = resolveAuthSecret(cfg.Env, cfg.Auth, &errs)

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}

	return cfg, nil
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
