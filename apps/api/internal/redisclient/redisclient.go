// Package redisclient builds the connection settings this service uses to
// reach Redis.
//
// It exists so that there is exactly one answer to "how does CollabBoard
// connect to Redis", rather than one per consumer. Redis is not a single
// dependency here: it backs the session and refresh-token store, the WebSocket
// pub/sub fan-out, the /healthz probe, and — when the job queue lands — Asynq,
// which takes its own asynq.RedisClientOpt rather than a *redis.Options. A
// second call site assembling a slightly different tls.Config is how a service
// ends up believing it is encrypted when one of its connections is not, so the
// TLS decision is made once, here, and both shapes are served from it.
package redisclient

import (
	"crypto/tls"
	"log/slog"

	"github.com/redis/go-redis/v9"

	"github.com/AndyV99/collabboard/apps/api/internal/config"
)

// Options renders the go-redis client settings for cfg.
func Options(cfg config.RedisConfig) *redis.Options {
	return &redis.Options{
		Addr:      cfg.Addr(),
		Password:  cfg.Password,
		DB:        cfg.DB,
		TLSConfig: TLSConfig(cfg),
	}
}

// TLSConfig renders the TLS settings for cfg, or nil when TLS is off.
//
// A fresh value per call, deliberately. Memoising it into a package variable
// would be the obvious optimisation and would hand every consumer the same
// mutable struct, so Asynq adjusting its copy would silently change the one the
// go-redis client is already using.
//
// Nil is meaningful rather than empty: go-redis and Asynq both treat a nil
// TLSConfig as "speak the plaintext protocol", so this returning nil is what
// keeps the local compose stack working, and it returning non-nil is the whole
// of what REDIS_TLS_ENABLED does.
//
// There is no InsecureSkipVerify escape hatch and there should not be one. A
// TLS connection that does not verify the certificate is an unauthenticated
// connection that still pays for a handshake — anything able to answer on the
// address gets to be Redis, which for this service means it gets to serve
// session records and refresh tokens. The failure mode that tempts people into
// it, a certificate error against a local proxy, is better solved by trusting
// that proxy's CA in the container than by turning verification off globally.
//
// RootCAs is deliberately left nil, meaning the system trust store. ElastiCache
// presents a certificate chaining to a public Amazon CA, so there is no bundle
// to ship; a private CA would be a RootCAs pool built here, not a skipped
// check.
func TLSConfig(cfg config.RedisConfig) *tls.Config {
	if !cfg.TLSEnabled {
		return nil
	}

	return &tls.Config{
		// The name the certificate must be valid for. Derived from the
		// configured host rather than left empty: crypto/tls would otherwise
		// fall back to whatever address was dialled, which is the same string
		// today and silently stops being so behind a proxy or an IP-literal
		// REDIS_HOST.
		ServerName: cfg.Host,

		// Pinned rather than inherited so the floor cannot drift with the Go
		// release. ElastiCache offers 1.2 and 1.3.
		MinVersion: tls.VersionTLS12,
	}
}

// New opens a Redis client for cfg.
//
// The logger records which transport was selected. It is one line at startup,
// and it is the only evidence available in a deployed task of whether this
// process thinks it is encrypted — the question that has to be answerable
// first when a task fails its health check against a TLS-only replication
// group. Address and TLS mode only; the password is never logged.
//
// It reports what was actually built rather than what was requested. Logging
// cfg.TLSEnabled would restate the input: if Options ever stopped honouring the
// flag, the line would still read tls_enabled=true while the socket was
// plaintext, which is precisely the failure this package exists to make
// impossible to have quietly.
func New(logger *slog.Logger, cfg config.RedisConfig) *redis.Client {
	opts := Options(cfg)

	logger.Info("redis client configured",
		slog.String("addr", opts.Addr),
		slog.Bool("tls_enabled", opts.TLSConfig != nil),
	)

	return redis.NewClient(opts)
}
