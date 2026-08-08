package store_test

// Test harness for the tests that need a real Postgres.
//
// The proper harness — Testcontainers, so CI gets a database of its own — is
// issue #7. Until that lands these tests use whatever Postgres the local
// compose stack is serving and skip when there is none, so `go test ./...` is
// green on a machine without a database and a genuine test on one with it. The
// alternative, asserting isolation against a mock, would test the mock.
//
// Two identities, deliberately. Queries under test run as the serving role
// (collabboard_app: no superuser, no BYPASSRLS, owns nothing), which is the only
// identity the policies actually apply to. Seeding runs as the migration role,
// because seeding is precisely the thing the policies forbid: a tenant-scoped
// connection cannot create a user at all, since the users policy requires a
// membership that cannot exist before the user does. That is not a gap in this
// test, it is issue #13 showing up exactly where the migration comment says it
// will.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/config"
)

// pingTimeout bounds the "is there a database?" probe. Short on purpose: when
// there is nothing listening the tests should skip promptly, not hang.
const pingTimeout = 3 * time.Second

// newPool opens a pool as the serving role, or skips the test if no database is
// reachable.
//
// maxConns is a parameter because some of these tests only mean something at
// exactly one connection: with a pool of one, "the pool gave the connection
// back" and "the next query ran on the same backend" are observable facts
// rather than probabilities.
func newPool(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()

	cfg := loadConfig(t)

	return openPool(t, cfg.Postgres.DSN(), maxConns)
}

// newOwnerPool opens a pool as the migration role, used only to seed fixtures.
func newOwnerPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	cfg := loadConfig(t)

	return openPool(t, cfg.Postgres.MigrationDSN(), 2)
}

func loadConfig(t *testing.T) config.Config {
	t.Helper()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("loading config: %v", err)
	}

	return cfg
}

func openPool(t *testing.T, dsn string, maxConns int32) *pgxpool.Pool {
	t.Helper()

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing dsn: %v", err)
	}

	poolCfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		t.Fatalf("creating pool: %v", err)
	}

	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()

	if err := pool.Ping(ctx); err != nil {
		t.Skipf("no reachable postgres (%v); start the compose stack to run this test", err)
	}

	return pool
}

// tenantFixture is one tenant's worth of seeded data: an organization, a member
// of its own, a project, a board, a column and a card.
type tenantFixture struct {
	label     string
	tenantID  uuid.UUID
	projectID uuid.UUID
	boardID   uuid.UUID
	columnID  uuid.UUID
	cardTitle string

	memberEmail string
}

// seedTenants creates two independent tenants plus one user who belongs to
// both. The shared user is the interesting part: it is what distinguishes "the
// query is filtered" from "the tenants happen not to overlap", and it exercises
// the membership-derived policy on the global users table in both directions.
func seedTenants(t *testing.T, owner *pgxpool.Pool) (a, b tenantFixture, sharedEmail string) {
	t.Helper()

	run := uuid.NewString()[:8]

	a = seedTenant(t, owner, "alpha-"+run)
	b = seedTenant(t, owner, "beta-"+run)

	sharedEmail = "shared-" + run + "@example.com"
	sharedID := uuid.New()

	exec(t, owner, `INSERT INTO users (id, email, display_name) VALUES ($1, $2, $3)`,
		sharedID, sharedEmail, "Shared Contractor "+run)

	for _, f := range []tenantFixture{a, b} {
		exec(t, owner, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'member')`,
			f.tenantID, sharedID)
	}

	t.Cleanup(func() {
		// Organizations cascade to memberships, projects, boards, columns and
		// cards. users is global, so it is outside that cascade and has to go
		// explicitly — the same asymmetry the schema is built around.
		for _, f := range []tenantFixture{a, b} {
			exec(t, owner, `DELETE FROM organizations WHERE id = $1`, f.tenantID)
		}

		exec(t, owner, `DELETE FROM users WHERE id = $1`, sharedID)
		exec(t, owner, `DELETE FROM users WHERE email = $1 OR email = $2`, a.memberEmail, b.memberEmail)
	})

	return a, b, sharedEmail
}

func seedTenant(t *testing.T, owner *pgxpool.Pool, label string) tenantFixture {
	t.Helper()

	f := tenantFixture{
		label:       label,
		tenantID:    uuid.New(),
		projectID:   uuid.New(),
		boardID:     uuid.New(),
		columnID:    uuid.New(),
		cardTitle:   label + " card",
		memberEmail: label + "-member@example.com",
	}

	userID := uuid.New()

	exec(t, owner, `INSERT INTO organizations (id, name, slug) VALUES ($1, $2, $3)`,
		f.tenantID, label+" org", label)
	exec(t, owner, `INSERT INTO users (id, email, display_name) VALUES ($1, $2, $3)`,
		userID, f.memberEmail, label+" member")
	exec(t, owner, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`,
		f.tenantID, userID)
	exec(t, owner, `INSERT INTO projects (id, tenant_id, name) VALUES ($1, $2, $3)`,
		f.projectID, f.tenantID, label+" project")
	exec(t, owner, `INSERT INTO boards (id, tenant_id, project_id, name) VALUES ($1, $2, $3, $4)`,
		f.boardID, f.tenantID, f.projectID, label+" board")
	exec(t, owner, `INSERT INTO columns (id, tenant_id, board_id, name, position) VALUES ($1, $2, $3, $4, 1)`,
		f.columnID, f.tenantID, f.boardID, "Todo")
	exec(t, owner, `INSERT INTO cards (tenant_id, board_id, column_id, title, position) VALUES ($1, $2, $3, $4, 1)`,
		f.tenantID, f.boardID, f.columnID, f.cardTitle)

	return f
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()

	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seeding (%s): %v", sql, err)
	}
}
