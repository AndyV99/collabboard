package config

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsMatchComposeStack(t *testing.T) {
	// Blank rather than unset, so the result does not depend on the developer's
	// ambient environment. Load treats blank and unset identically.
	for _, key := range []string{
		"HTTP_HOST", "HTTP_PORT",
		"POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_USER", "POSTGRES_DB",
		"REDIS_HOST", "REDIS_PORT", "REDIS_DB",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if got, want := cfg.HTTP.Addr(), ":8080"; got != want {
		t.Errorf("HTTP.Addr() = %q, want %q", got, want)
	}

	if cfg.Postgres.Host != "localhost" || cfg.Postgres.Port != 5432 {
		t.Errorf("Postgres = %s:%d, want localhost:5432", cfg.Postgres.Host, cfg.Postgres.Port)
	}

	if got, want := cfg.Redis.Addr(), "localhost:6379"; got != want {
		t.Errorf("Redis.Addr() = %q, want %q", got, want)
	}

	if got, want := cfg.Postgres.Database, "collabboard"; got != want {
		t.Errorf("Postgres.Database = %q, want %q", got, want)
	}
}

// The serving role and the migrating role must not be the same identity by
// default: the API connecting as the schema owner is the specific mistake that
// would make every RLS policy decorative.
func TestServingAndMigratingIdentitiesDiffer(t *testing.T) {
	for _, key := range []string{
		"POSTGRES_USER", "POSTGRES_PASSWORD",
		"POSTGRES_MIGRATION_USER", "POSTGRES_MIGRATION_PASSWORD",
	} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if got, want := cfg.Postgres.User, "collabboard_app"; got != want {
		t.Errorf("Postgres.User = %q, want %q — the API must not connect as the owner", got, want)
	}

	if cfg.Postgres.User == cfg.Postgres.MigrationUser {
		t.Errorf("serving and migrating roles are both %q", cfg.Postgres.User)
	}

	if dsn, migrationDSN := cfg.Postgres.DSN(), cfg.Postgres.MigrationDSN(); dsn == migrationDSN {
		t.Errorf("DSN() and MigrationDSN() are identical: %q", dsn)
	}
}

func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("HTTP_PORT", "9999")
	t.Setenv("POSTGRES_HOST", "db.internal")
	t.Setenv("REDIS_DB", "3")
	t.Setenv("HTTP_SHUTDOWN_TIMEOUT", "45s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if cfg.HTTP.Port != 9999 {
		t.Errorf("HTTP.Port = %d, want 9999", cfg.HTTP.Port)
	}

	if cfg.Postgres.Host != "db.internal" {
		t.Errorf("Postgres.Host = %q, want %q", cfg.Postgres.Host, "db.internal")
	}

	if cfg.Redis.DB != 3 {
		t.Errorf("Redis.DB = %d, want 3", cfg.Redis.DB)
	}

	if cfg.HTTP.ShutdownTimeout != 45*time.Second {
		t.Errorf("HTTP.ShutdownTimeout = %s, want 45s", cfg.HTTP.ShutdownTimeout)
	}
}

func TestLoadRejectsMalformedValues(t *testing.T) {
	t.Setenv("HTTP_PORT", "not-a-port")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a non-integer HTTP_PORT, got nil")
	}
}

// A password containing URL-significant characters must survive into the DSN
// intact rather than corrupting it.
func TestDSNEscapesPassword(t *testing.T) {
	p := PostgresConfig{
		Host:     "localhost",
		Port:     5432,
		User:     "collabboard",
		Password: "p@ss:w/rd?",
		Database: "collabboard",
		SSLMode:  "disable",
	}

	dsn := p.DSN()

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("DSN %q is not a valid URL: %v", dsn, err)
	}

	got, set := parsed.User.Password()
	if !set || got != p.Password {
		t.Errorf("password round-tripped as %q (set=%t), want %q", got, set, p.Password)
	}

	if !strings.Contains(dsn, "sslmode=disable") {
		t.Errorf("DSN %q is missing sslmode", dsn)
	}
}

// REDIS_TLS_ENABLED is a security switch, so an unparseable value has to stop
// the service rather than quietly resolve to the default. "yes" is the
// realistic typo: an operator writes it meaning to turn encryption on, and
// falling back to false would hand them a plaintext client that reports itself
// as configured.
func TestRedisTLSRejectsUnparseableValue(t *testing.T) {
	// " true" is the same class of mistake arriving by a different route: a
	// YAML block scalar or a copied task-definition value that keeps its
	// leading space. ParseBool rejects it, and it must stay rejected rather
	// than being trimmed into working, because trimming here and not elsewhere
	// is its own inconsistency.
	for _, raw := range []string{"yes", "on", "enabled", " true"} {
		t.Run("REDIS_TLS_ENABLED="+raw, func(t *testing.T) {
			t.Setenv("REDIS_TLS_ENABLED", raw)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load() accepted REDIS_TLS_ENABLED=%q; it must refuse a value it cannot parse", raw)
			}

			if !strings.Contains(err.Error(), "REDIS_TLS_ENABLED") {
				t.Errorf("error %q does not name the offending variable", err)
			}
		})
	}
}

// The accepted spellings, and the default. Off is what the local compose stack
// needs; on is what ElastiCache needs.
func TestRedisTLSParsing(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{raw: "", want: false},
		{raw: "false", want: false},
		{raw: "0", want: false},
		{raw: "true", want: true},
		{raw: "1", want: true},
		{raw: "TRUE", want: true},
	} {
		t.Run("REDIS_TLS_ENABLED="+tc.raw, func(t *testing.T) {
			t.Setenv("REDIS_TLS_ENABLED", tc.raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() returned error: %v", err)
			}

			if got := cfg.Redis.TLSEnabled; got != tc.want {
				t.Errorf("Redis.TLSEnabled = %t, want %t", got, tc.want)
			}
		})
	}
}

// sslmode=verify-full is only as good as the CA file libpq resolves, and libpq
// resolves it from sslrootcert — defaulting to ~/.postgresql/root.crt, which
// does not exist in a distroless image. Before POSTGRES_SSLROOTCERT existed,
// verify-full was documented in .env.example and unreachable in practice: the
// connection failed with "root certificate file ... does not exist", which
// reads like a broken image rather than like a missing setting.
//
// Both DSNs, because `api migrate` connects with MigrationDSN and would
// otherwise be the one connection in the system still unverified — and it is
// the one holding the schema owner's credential.
func TestSSLRootCertReachesBothDSNs(t *testing.T) {
	const path = "/etc/ssl/certs/rds-global-bundle.pem"

	p := PostgresConfig{
		Host:              "collabboard-staging-postgres.abcdef.us-east-1.rds.amazonaws.com",
		Port:              5432,
		User:              "collabboard_app",
		Password:          "serving",
		Database:          "collabboard",
		SSLMode:           "verify-full",
		SSLRootCert:       path,
		MigrationUser:     "collabboard_owner",
		MigrationPassword: "owner",
	}

	for name, dsn := range map[string]string{"DSN": p.DSN(), "MigrationDSN": p.MigrationDSN()} {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("%s %q is not a valid URL: %v", name, dsn, err)
		}

		// Read the parsed query rather than substring-matching the string: a
		// path is percent-encoded on the wire, so strings.Contains(dsn, path)
		// would pass or fail for reasons unrelated to the value libpq sees.
		if got := parsed.Query().Get("sslrootcert"); got != path {
			t.Errorf("%s: sslrootcert = %q, want %q", name, got, path)
		}

		if got := parsed.Query().Get("sslmode"); got != "verify-full" {
			t.Errorf("%s: sslmode = %q, want verify-full", name, got)
		}
	}
}

// The other half, and the one that protects the local loop. An empty
// SSLRootCert must omit the parameter rather than emit it empty: libpq reads
// `sslrootcert=` as "no CA file", which under verify-full fails the connection
// outright instead of falling back to a default. Emitting it empty would also
// change the DSN every local developer has been running against, for a setting
// none of them set.
func TestSSLRootCertIsOmittedWhenUnset(t *testing.T) {
	p := PostgresConfig{
		Host:              "localhost",
		Port:              5432,
		User:              "collabboard_app",
		Password:          "dev",
		Database:          "collabboard",
		SSLMode:           "disable",
		MigrationUser:     "collabboard_owner",
		MigrationPassword: "dev",
	}

	for name, dsn := range map[string]string{"DSN": p.DSN(), "MigrationDSN": p.MigrationDSN()} {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("%s %q is not a valid URL: %v", name, dsn, err)
		}

		if _, present := parsed.Query()["sslrootcert"]; present {
			t.Errorf("%s: sslrootcert must be absent when unset, but %q carries it", name, dsn)
		}
	}
}

// Load has to read it, not merely carry it. A field wired into the struct and
// not into Load is the exact shape of this issue's original defect: a setting
// that exists in the type, is documented, and cannot be set.
func TestLoadReadsSSLRootCert(t *testing.T) {
	const path = "/etc/ssl/certs/rds-global-bundle.pem"

	t.Setenv("POSTGRES_SSLROOTCERT", path)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Postgres.SSLRootCert != path {
		t.Errorf("SSLRootCert = %q, want %q", cfg.Postgres.SSLRootCert, path)
	}
}
