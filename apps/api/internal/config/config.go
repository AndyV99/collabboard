// Package config loads service configuration from the process environment.
//
// Every value has a default that matches the local docker-compose stack, so a
// fresh checkout runs with no environment set at all. Credentials are never
// hardcoded to anything but those local dev values, and production supplies all
// of them through the environment.
package config

import (
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
type PostgresConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string

	MaxConns int32
}

// DSN renders a libpq-style connection URL. The password is URL-escaped rather
// than interpolated so that special characters in a real secret cannot corrupt
// the DSN.
func (p PostgresConfig) DSN() string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(p.User, p.Password),
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
			Host:     envString("POSTGRES_HOST", "localhost"),
			Port:     envInt("POSTGRES_PORT", 5432, &errs),
			User:     envString("POSTGRES_USER", "collabboard"),
			Password: envString("POSTGRES_PASSWORD", "dev"),
			Database: envString("POSTGRES_DB", "collabboard"),
			SSLMode:  envString("POSTGRES_SSLMODE", "disable"),
			MaxConns: int32(envInt("POSTGRES_MAX_CONNS", 10, &errs)), //nolint:gosec // bounded small int from config
		},
		Redis: RedisConfig{
			Host:     envString("REDIS_HOST", "localhost"),
			Port:     envInt("REDIS_PORT", 6379, &errs),
			Password: envString("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0, &errs),
		},
	}

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %s", strings.Join(errs, "; "))
	}

	return cfg, nil
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
