package config

// The realtime settings, and the one of them that is a security boundary.

import (
	"slices"
	"testing"
	"time"
)

func TestRealtimeDefaultsAreUsable(t *testing.T) {
	for _, key := range []string{
		"REALTIME_SEND_BUFFER", "REALTIME_PING_INTERVAL", "REALTIME_PONG_TIMEOUT",
		"REALTIME_WRITE_TIMEOUT", "REALTIME_READ_LIMIT_BYTES", "REALTIME_REAUTHORIZE_INTERVAL",
		"REALTIME_MAX_BOARDS_PER_CONNECTION", "REALTIME_BROKER_BUFFER",
		"REALTIME_SHUTDOWN_RECONNECT_HINT",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	rt := cfg.Realtime

	// The ping interval has to stay comfortably under the 60s idle timeout an
	// AWS ALB applies by default, or the load balancer reaps healthy
	// connections on a quiet board.
	if rt.PingInterval <= 0 || rt.PingInterval >= 60*time.Second {
		t.Errorf("PingInterval = %s, want something under the 60s ALB idle timeout", rt.PingInterval)
	}

	if rt.PongTimeout <= 0 || rt.PongTimeout >= rt.PingInterval {
		t.Errorf("PongTimeout = %s, want a positive value shorter than the ping interval (%s)",
			rt.PongTimeout, rt.PingInterval)
	}

	// A revoked membership survives at most one sweep, and the sweep has to be
	// well inside the access token's life or it would never run before the
	// connection was closed for expiry anyway.
	if rt.ReauthorizeInterval <= 0 || rt.ReauthorizeInterval >= cfg.Auth.AccessTokenTTL {
		t.Errorf("ReauthorizeInterval = %s, want a positive value under the access token TTL (%s)",
			rt.ReauthorizeInterval, cfg.Auth.AccessTokenTTL)
	}

	for name, value := range map[string]int{
		"SendBuffer":            rt.SendBuffer,
		"ReadLimit":             rt.ReadLimit,
		"MaxRoomsPerConnection": rt.MaxRoomsPerConnection,
		"BrokerBuffer":          rt.BrokerBuffer,
	} {
		if value <= 0 {
			t.Errorf("%s = %d, want a positive value", name, value)
		}
	}
}

// TestAllowedOriginsDefaultsClosedOutsideDevelopment is the security assertion.
//
// An origin allow-list that defaults to open is an allow-list nobody ever
// configures, so unset means same-origin only everywhere but the local loop.
func TestAllowedOriginsDefaultsClosedOutsideDevelopment(t *testing.T) {
	for _, env := range []string{"production", "staging"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("APP_ENV", env)
			t.Setenv("AUTH_JWT_SECRET", "0123456789abcdef0123456789abcdef")
			t.Setenv("REALTIME_ALLOWED_ORIGINS", "")

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}

			if len(cfg.Realtime.AllowedOrigins) != 0 {
				t.Fatalf("AllowedOrigins = %v outside development, want empty (same-origin only)",
					cfg.Realtime.AllowedOrigins)
			}

			if slices.Contains(cfg.Realtime.AllowedOrigins, "*") {
				t.Fatal("the default allow-list is open")
			}
		})
	}
}

func TestAllowedOriginsDefaultsToTheLocalWebAppInDevelopment(t *testing.T) {
	t.Setenv("APP_ENV", EnvDevelopment)
	t.Setenv("REALTIME_ALLOWED_ORIGINS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if !slices.Contains(cfg.Realtime.AllowedOrigins, "localhost:3000") {
		t.Errorf("AllowedOrigins = %v in development, want the local Next.js origin",
			cfg.Realtime.AllowedOrigins)
	}
}

func TestAllowedOriginsAreParsedFromTheEnvironment(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("AUTH_JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("REALTIME_ALLOWED_ORIGINS", " app.collabboard.dev , ,staging.collabboard.dev ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	want := []string{"app.collabboard.dev", "staging.collabboard.dev"}
	if !slices.Equal(cfg.Realtime.AllowedOrigins, want) {
		t.Errorf("AllowedOrigins = %v, want %v", cfg.Realtime.AllowedOrigins, want)
	}
}

func TestRealtimeRejectsUnparseableValues(t *testing.T) {
	t.Setenv("REALTIME_PING_INTERVAL", "soon")
	t.Setenv("REALTIME_SEND_BUFFER", "lots")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() accepted an unparseable realtime configuration")
	}

	t.Logf("Load() refused: %v", err)
}
