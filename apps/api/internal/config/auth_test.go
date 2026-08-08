package config

// The one configuration value with no safe default.
//
// Everything else in AuthConfig is a tuning knob whose wrong setting makes the
// service slow or annoying. AUTH_JWT_SECRET's wrong setting makes every access
// token forgeable, and a forgeable access token is a forgeable *tenant claim* —
// which is the one thing standing between one customer and another's data (see
// internal/api/auth_bola_test.go). So the interesting tests here are about
// refusing to start.

import (
	"strings"
	"testing"
	"time"
)

func TestAuthSecretIsRequiredOutsideDevelopment(t *testing.T) {
	for _, env := range []string{"production", "staging", "test", "anything-else"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("APP_ENV", env)
			t.Setenv("AUTH_JWT_SECRET", "")

			_, err := Load()

			t.Logf("APP_ENV=%s with no secret -> %v", env, err)

			if err == nil {
				t.Fatal("Load succeeded with no signing secret; every token would be signed with an empty key")
			}

			if !strings.Contains(err.Error(), "AUTH_JWT_SECRET") {
				t.Errorf("the error does not name the variable an operator has to set: %v", err)
			}
		})
	}
}

func TestAuthSecretMustBeLongEnoughForHS256(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("AUTH_JWT_SECRET", "far-too-short")

	_, err := Load()

	t.Logf("a 13-byte secret -> %v", err)

	if err == nil {
		// HS256 is HMAC-SHA256: the signature is only as strong as the key's
		// entropy, so a short secret is brute-forceable offline by anyone
		// holding one valid token.
		t.Fatal("Load accepted a secret shorter than 32 bytes")
	}
}

// TestDevelopmentGeneratesAFreshSecret documents the deliberate absence of a
// committed default.
//
// A constant in a checked-in file is a real credential the moment someone
// copies the compose stack onto a shared host, and it would verify tokens
// minted by anyone who has read this repository. Randomising per process costs
// exactly one thing — a local restart invalidates local tokens — which is the
// right amount of inconvenience.
func TestDevelopmentGeneratesAFreshSecret(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("AUTH_JWT_SECRET", "")

	first, err := Load()
	if err != nil {
		t.Fatalf("Load in development: %v", err)
	}

	second, err := Load()
	if err != nil {
		t.Fatalf("Load in development (second call): %v", err)
	}

	t.Logf("generated secrets are %d bytes and differ: %t",
		len(first.Auth.JWTSecret), first.Auth.JWTSecret != second.Auth.JWTSecret)

	if len(first.Auth.JWTSecret) < minJWTSecretLength {
		t.Errorf("generated secret is %d bytes, want at least %d", len(first.Auth.JWTSecret), minJWTSecretLength)
	}

	if first.Auth.JWTSecret == second.Auth.JWTSecret {
		t.Error("two loads produced the same development secret; it is a constant, not a generated value")
	}
}

func TestAuthDefaults(t *testing.T) {
	t.Setenv("APP_ENV", "development")

	for _, key := range []string{
		"AUTH_JWT_SECRET", "AUTH_TOKEN_ISSUER", "AUTH_TOKEN_AUDIENCE",
		"AUTH_ACCESS_TOKEN_TTL", "AUTH_REFRESH_TOKEN_TTL",
		"AUTH_ARGON2_MEMORY_KIB", "AUTH_LOGIN_RATE_PER_ACCOUNT", "AUTH_LOGIN_RATE_WINDOW",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Logf("access ttl=%s refresh ttl=%s argon2 memory=%d KiB per-account budget=%d/%s",
		cfg.Auth.AccessTokenTTL, cfg.Auth.RefreshTokenTTL,
		cfg.Auth.Argon2MemoryKiB, cfg.Auth.LoginRatePerAccount, cfg.Auth.LoginRateWindow)

	if cfg.Auth.AccessTokenTTL != 15*time.Minute {
		t.Errorf("AccessTokenTTL = %s, want 15m", cfg.Auth.AccessTokenTTL)
	}

	// The access token cannot be revoked, so its lifetime is the window in
	// which a revoked session still works. A default measured in hours would
	// quietly undo the refresh-token revocation this service went to the
	// trouble of implementing.
	if cfg.Auth.AccessTokenTTL > time.Hour {
		t.Errorf("AccessTokenTTL = %s; an unrevocable credential should not last that long", cfg.Auth.AccessTokenTTL)
	}

	if cfg.Auth.RefreshTokenTTL <= cfg.Auth.AccessTokenTTL {
		t.Errorf("RefreshTokenTTL (%s) is not longer than AccessTokenTTL (%s); the pairing makes no sense",
			cfg.Auth.RefreshTokenTTL, cfg.Auth.AccessTokenTTL)
	}

	// RFC 9106's second recommended parameter set, which is also the floor the
	// CHECK constraints in migration 00005 enforce.
	if cfg.Auth.Argon2MemoryKiB != 19456 || cfg.Auth.Argon2Iterations != 2 {
		t.Errorf("argon2 defaults = %d KiB / %d iterations, want 19456 / 2",
			cfg.Auth.Argon2MemoryKiB, cfg.Auth.Argon2Iterations)
	}

	if cfg.Auth.LoginRatePerAccount >= cfg.Auth.LoginRatePerAddress {
		t.Errorf("the per-account budget (%d) is not tighter than the per-address one (%d); an address behind a NAT would lock out real users first",
			cfg.Auth.LoginRatePerAccount, cfg.Auth.LoginRatePerAddress)
	}
}
