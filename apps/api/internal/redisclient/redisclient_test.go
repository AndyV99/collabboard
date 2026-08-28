package redisclient_test

import (
	"crypto/tls"
	"io"
	"log/slog"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/internal/config"
	"github.com/AndyV99/collabboard/apps/api/internal/redisclient"
)

func baseConfig() config.RedisConfig {
	return config.RedisConfig{
		Host:     "collabboard.abc123.ng.0001.use1.cache.amazonaws.com",
		Port:     6379,
		Password: "",
		DB:       0,
	}
}

// The bug this package exists to fix: go-redis speaks TLS only when
// Options.TLSConfig is non-nil, so a nil one against an ElastiCache replication
// group created with transit_encryption_enabled = true is not a degraded
// connection, it is no connection at all.
func TestOptionsTLSConfigFollowsTheSetting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		enabled bool
		wantTLS bool
	}{
		{name: "disabled matches the local compose stack", enabled: false, wantTLS: false},
		{name: "enabled matches ElastiCache in transit encryption", enabled: true, wantTLS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			cfg.TLSEnabled = tc.enabled

			opts := redisclient.Options(cfg)

			if got := opts.TLSConfig != nil; got != tc.wantTLS {
				t.Fatalf("Options(TLSEnabled=%t).TLSConfig non-nil = %t, want %t",
					tc.enabled, got, tc.wantTLS)
			}
		})
	}
}

// ServerName has to be the configured host, because that is what the
// certificate ElastiCache presents is issued for. Leaving it empty makes
// crypto/tls fall back to the dial address, which happens to be the same string
// today and would stop being so the moment anything routes through a proxy.
func TestOptionsVerifiesTheServerItAskedFor(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.TLSEnabled = true

	opts := redisclient.Options(cfg)

	if opts.TLSConfig == nil {
		t.Fatal("TLSConfig is nil with TLSEnabled = true")
	}

	if got, want := opts.TLSConfig.ServerName, cfg.Host; got != want {
		t.Errorf("ServerName = %q, want %q", got, want)
	}
}

// The assertion the rest of this package is in service of. InsecureSkipVerify
// turns a TLS connection into an unauthenticated one that still costs a
// handshake: anything able to answer on the address gets to be Redis. Nothing
// in config can reach this field, and this test is what keeps that true when
// someone hits a certificate error against a self-signed local proxy and
// reaches for the quickest way through it.
func TestTLSNeverSkipsVerification(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		cfg := baseConfig()
		cfg.TLSEnabled = enabled

		if tlsCfg := redisclient.TLSConfig(cfg); tlsCfg != nil && tlsCfg.InsecureSkipVerify {
			t.Errorf("TLSConfig(TLSEnabled=%t).InsecureSkipVerify is true", enabled)
		}

		if opts := redisclient.Options(cfg); opts.TLSConfig != nil && opts.TLSConfig.InsecureSkipVerify {
			t.Errorf("Options(TLSEnabled=%t).TLSConfig.InsecureSkipVerify is true", enabled)
		}
	}
}

// Asynq takes an asynq.RedisClientOpt with its own TLSConfig field rather than
// a *redis.Options, so the exported builder is what the job queue will call.
// If these two disagree, half the service is encrypted.
//
// This asserts *delegation*, not field parity: Options calls TLSConfig today,
// so the only way to fail is to stop doing that. Adding a field to TLSConfig
// will not make this test say anything new, and it is not meant to.
func TestTLSConfigAndOptionsAgree(t *testing.T) {
	t.Parallel()

	for _, enabled := range []bool{false, true} {
		cfg := baseConfig()
		cfg.TLSEnabled = enabled

		var (
			standalone = redisclient.TLSConfig(cfg)
			viaOptions = redisclient.Options(cfg).TLSConfig
		)

		if (standalone == nil) != (viaOptions == nil) {
			t.Fatalf("TLSEnabled=%t: TLSConfig() nil = %t but Options().TLSConfig nil = %t",
				enabled, standalone == nil, viaOptions == nil)
		}

		if standalone == nil {
			continue
		}

		if standalone.ServerName != viaOptions.ServerName {
			t.Errorf("TLSEnabled=%t: ServerName %q via TLSConfig() vs %q via Options()",
				enabled, standalone.ServerName, viaOptions.ServerName)
		}

		if standalone.MinVersion != viaOptions.MinVersion {
			t.Errorf("TLSEnabled=%t: MinVersion %d via TLSConfig() vs %d via Options()",
				enabled, standalone.MinVersion, viaOptions.MinVersion)
		}
	}
}

// TLS 1.0 and 1.1 are deprecated and ElastiCache does not offer them. Pinning
// the floor explicitly rather than inheriting crypto/tls's default keeps the
// guarantee from moving under the service when the Go version changes.
func TestTLSMinimumVersion(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	cfg.TLSEnabled = true

	tlsCfg := redisclient.TLSConfig(cfg)
	if tlsCfg == nil {
		t.Fatal("TLSConfig is nil with TLSEnabled = true")
	}

	if tlsCfg.MinVersion < tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want at least TLS 1.2 (%d)", tlsCfg.MinVersion, tls.VersionTLS12)
	}
}

// Moving construction out of cmd/api made these three regressable in a way they
// were not when they sat inline next to redis.NewClient. Nothing else would
// catch a dropped Password: the compose stack and the Testcontainers harness
// both run Redis without auth, so the integration suite is blind to it and only
// a deployed environment would notice.
func TestOptionsCarriesConnectionSettings(t *testing.T) {
	t.Parallel()

	cfg := baseConfig()
	// Not a credential: this hostname does not resolve and this password
	// authenticates against nothing. It exists only to be compared to itself.
	cfg.Password = "not-a-real-password"
	cfg.DB = 3

	opts := redisclient.Options(cfg)

	if got, want := opts.Addr, cfg.Addr(); got != want {
		t.Errorf("Addr = %q, want %q", got, want)
	}

	if got, want := opts.Password, cfg.Password; got != want {
		t.Errorf("Password = %q, want %q", got, want)
	}

	if got, want := opts.DB, cfg.DB; got != want {
		t.Errorf("DB = %d, want %d", got, want)
	}
}

// New is the only function cmd/api calls, so it is the one whose behaviour
// actually ships. Options being correct does not prove New uses it.
func TestNewAppliesTheSameOptions(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, enabled := range []bool{false, true} {
		cfg := baseConfig()
		cfg.TLSEnabled = enabled

		client := redisclient.New(logger, cfg)

		t.Cleanup(func() { _ = client.Close() })

		opts := client.Options()

		if got := opts.TLSConfig != nil; got != enabled {
			t.Errorf("New(TLSEnabled=%t) built TLSConfig non-nil = %t", enabled, got)
		}

		if got, want := opts.Addr, cfg.Addr(); got != want {
			t.Errorf("New(TLSEnabled=%t) Addr = %q, want %q", enabled, got, want)
		}
	}
}

// The passwordless local default must not start emitting a TLS handshake, or
// `docker compose up` breaks for every developer at once.
func TestDefaultConfigIsPlaintext(t *testing.T) {
	// No t.Parallel: t.Setenv mutates process state, and blank rather than
	// unset so the result does not depend on the developer's ambient
	// environment.
	t.Setenv("REDIS_TLS_ENABLED", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load() returned error: %v", err)
	}

	if cfg.Redis.TLSEnabled {
		t.Error("Redis.TLSEnabled defaults to true; the local compose stack has no TLS")
	}

	if opts := redisclient.Options(cfg.Redis); opts.TLSConfig != nil {
		t.Error("default configuration builds a TLS client")
	}
}
