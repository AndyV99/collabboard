//go:build integration

package store_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// errBoom stands in for "the callback failed for its own reasons".
var errBoom = errors.New("boom")

// TestWithTenantSeesOnlyItsOwnTenantsRows is the claim the whole isolation model
// rests on: the same query, run through the same helper against the same
// connection pool, returns different rows depending only on the tenant it was
// scoped to — with no tenant predicate anywhere in the SQL.
func TestWithTenantSeesOnlyItsOwnTenantsRows(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 4)
	superuser := newSuperuserPool(t)
	a, b, sharedEmail := seedTenants(t, superuser)
	s := store.New(pool)

	for _, tc := range []struct {
		self  tenantFixture
		other tenantFixture
	}{
		{self: a, other: b},
		{self: b, other: a},
	} {
		t.Run(tc.self.Label, func(t *testing.T) {
			err := s.WithTenant(ctx, tc.self.TenantID, func(ctx context.Context, q store.Querier) error {
				projects, err := q.ListProjects(ctx)
				if err != nil {
					return err
				}

				t.Logf("tenant %s: ListProjects -> %s", tc.self.Label, projectNames(projects))

				if len(projects) != 1 || projects[0].ID != tc.self.ProjectID {
					t.Errorf("ListProjects = %v, want exactly the %s project", projects, tc.self.Label)
				}

				// The other tenant's board, addressed by its real primary key.
				// RLS turns a cross-tenant read into "no such row" rather than
				// into someone else's data.
				_, err = q.GetBoard(ctx, tc.other.BoardID)
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Errorf("GetBoard(%s's board) = %v, want pgx.ErrNoRows", tc.other.Label, err)
				}

				t.Logf("tenant %s: GetBoard(%s's board id) -> %v", tc.self.Label, tc.other.Label, err)

				cards, err := q.ListCardsByBoard(ctx, tc.other.BoardID)
				if err != nil {
					return err
				}

				if len(cards) != 0 {
					t.Errorf("ListCardsByBoard(%s's board) returned %d cards, want 0", tc.other.Label, len(cards))
				}

				t.Logf("tenant %s: ListCardsByBoard(%s's board id) -> %d cards", tc.self.Label, tc.other.Label, len(cards))

				// memberships is tenant-scoped, users is global with a policy
				// derived from memberships. Both halves have to hold: the
				// tenant's own member plus the contractor who belongs to both,
				// and nobody from the other tenant.
				members, err := q.ListMembers(ctx)
				if err != nil {
					return err
				}

				emails := memberEmails(members)
				t.Logf("tenant %s: ListMembers -> %v", tc.self.Label, emails)

				want := []string{sharedEmail, tc.self.MemberEmail}
				sort.Strings(want)

				if !slices.Equal(emails, want) {
					t.Errorf("ListMembers = %v, want %v", emails, want)
				}

				return nil
			})
			if err != nil {
				t.Fatalf("WithTenant: %v", err)
			}
		})
	}
}

// TestGetUserIsBoundedByTheDerivedUsersPolicy settles the question issue #75
// turned on: can a *tenant-scoped* transaction read a user row at all?
//
// It can, and the boundary is not the one the other tables have. `users` is
// global and carries no tenant_id; users_visible_via_membership makes a row
// visible only when a membership joins it to the current tenant. So the answer
// is neither "always" nor "never" — a member of this organization is readable
// by primary key, and anyone else is `no rows`, including a real account with a
// real id that simply belongs elsewhere.
//
// That is the whole reason GET /me does not need the pre-tenant door: the
// caller is by definition a member of the tenant in their own token, so their
// own row is on the visible side of this policy.
func TestGetUserIsBoundedByTheDerivedUsersPolicy(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 4)
	superuser := newSuperuserPool(t)
	fixture := seedFixture(t, superuser)
	s := store.New(pool)

	for _, tc := range []struct {
		self  tenantFixture
		other tenantFixture
	}{
		{self: fixture.A, other: fixture.B},
		{self: fixture.B, other: fixture.A},
	} {
		t.Run(tc.self.Label, func(t *testing.T) {
			err := s.WithTenant(ctx, tc.self.TenantID, func(ctx context.Context, q store.Querier) error {
				// 1. The tenant's own member, by primary key. This is the /me
				//    case: the caller reading themselves.
				own, err := q.GetUser(ctx, tc.self.MemberID)
				if err != nil {
					return err
				}

				t.Logf("tenant %s: GetUser(own member) -> %s / %q", tc.self.Label, own.Email, own.DisplayName)

				if own.ID != tc.self.MemberID || own.Email != tc.self.MemberEmail {
					t.Errorf("GetUser(own member) = %v, want %s / %s", own, tc.self.MemberID, tc.self.MemberEmail)
				}

				if own.DisplayName == "" {
					t.Error("GetUser returned an empty display name; /me would render nothing")
				}

				// 2. The user who belongs to both organizations. Visible here
				//    too, because a membership joins them to *this* tenant —
				//    the policy is about the membership, not about the row.
				shared, err := q.GetUser(ctx, fixture.SharedUserID)
				if err != nil {
					return err
				}

				t.Logf("tenant %s: GetUser(shared user) -> %s", tc.self.Label, shared.Email)

				if shared.Email != fixture.SharedEmail {
					t.Errorf("GetUser(shared user) = %v, want %s", shared, fixture.SharedEmail)
				}

				// 3. The other tenant's member, addressed by their real primary
				//    key. No row — not their address, and not an error that
				//    distinguishes "exists elsewhere" from "does not exist".
				foreign, err := q.GetUser(ctx, tc.other.MemberID)

				t.Logf("tenant %s: GetUser(%s's member id) -> %v", tc.self.Label, tc.other.Label, err)

				if !errors.Is(err, pgx.ErrNoRows) {
					t.Errorf("GetUser(%s's member) = %v, %v; want pgx.ErrNoRows", tc.other.Label, foreign, err)
				}

				return nil
			})
			if err != nil {
				t.Fatalf("WithTenant: %v", err)
			}
		})
	}
}

// TestSameQueryOutsideWithTenantSeesNothing covers the two halves of the bypass
// question at once, on a pool of exactly one connection so that everything below
// provably happens on the same Postgres backend.
//
//   - A query issued straight against the pool — the shape a handler would reach
//     for if it somehow got hold of one — returns zero rows, because
//     current_tenant_id() is NULL and every policy fails closed.
//   - After WithTenant returns, that same backend has no app.tenant_id left.
//     This is what SET LOCAL buys: the setting dies with the transaction, so a
//     pooled connection cannot carry one tenant into the next request.
func TestSameQueryOutsideWithTenantSeesNothing(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 1)
	superuser := newSuperuserPool(t)
	a, _, _ := seedTenants(t, superuser)
	s := store.New(pool)

	backendBefore := backendPID(t, pool)

	var seen int

	if err := s.WithTenant(ctx, a.TenantID, func(ctx context.Context, q store.Querier) error {
		projects, err := q.ListProjects(ctx)
		seen = len(projects)

		return err
	}); err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	t.Logf("inside WithTenant(%s): %d project(s) visible", a.Label, seen)

	if seen == 0 {
		t.Fatal("the tenant saw none of its own rows; the rest of this test would prove nothing")
	}

	var (
		backendAfter int
		tenantGUC    string
		projects     int
	)

	// One statement so there is no doubt the three facts describe the same
	// moment on the same connection.
	err := pool.QueryRow(ctx, `
		SELECT pg_backend_pid(),
		       coalesce(current_setting('app.tenant_id', true), ''),
		       (SELECT count(*) FROM projects)
	`).Scan(&backendAfter, &tenantGUC, &projects)
	if err != nil {
		t.Fatalf("querying outside WithTenant: %v", err)
	}

	t.Logf("outside WithTenant: backend pid %d (was %d), app.tenant_id=%q, projects visible: %d",
		backendAfter, backendBefore, tenantGUC, projects)

	if backendAfter != backendBefore {
		t.Fatalf("backend pid changed (%d -> %d); the pool handed out a different connection, so this proves nothing",
			backendBefore, backendAfter)
	}

	if tenantGUC != "" {
		t.Errorf("app.tenant_id = %q after WithTenant returned, want empty: SET LOCAL leaked past the transaction", tenantGUC)
	}

	if projects != 0 {
		t.Errorf("a query outside WithTenant saw %d projects, want 0", projects)
	}
}

// TestConnectionIsReturnedToThePool is the check most likely to catch a real
// bug, so it is deliberately strict: a pool of one, and NewConnsCount as the
// assertion. AcquiredConns returning to zero is necessary but not sufficient —
// pgxpool destroys rather than reuses a connection released while its
// transaction is still open, which would also read as zero acquired. If the
// helper ever leaked a transaction, the next acquire would have to build a new
// connection and NewConnsCount would move.
func TestConnectionIsReturnedToThePool(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 1)
	s := store.New(pool)

	// Warm up so the baseline describes an established connection rather than
	// an empty pool.
	backendPID(t, pool)
	assertPoolIdle(t, pool, "baseline")

	baseline := pool.Stat().NewConnsCount()

	t.Run("success", func(t *testing.T) {
		if err := s.WithTenant(ctx, uuid.New(), func(ctx context.Context, q store.Querier) error {
			_, err := q.ListProjects(ctx)

			return err
		}); err != nil {
			t.Fatalf("WithTenant: %v", err)
		}

		assertPoolIdle(t, pool, "after a successful call")
		assertNoNewConns(t, pool, baseline, "after a successful call")
	})

	t.Run("callback error", func(t *testing.T) {
		err := s.WithTenant(ctx, uuid.New(), func(ctx context.Context, q store.Querier) error {
			if _, qerr := q.ListProjects(ctx); qerr != nil {
				return qerr
			}

			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("WithTenant error = %v, want %v", err, errBoom)
		}

		assertPoolIdle(t, pool, "after a callback error")
		assertNoNewConns(t, pool, baseline, "after a callback error")
	})

	t.Run("failed statement inside the callback", func(t *testing.T) {
		// The nastiest of the four: a statement error puts the backend in a
		// failed transaction, and a connection released in that state is
		// destroyed rather than reused. Only an explicit rollback before the
		// release keeps it.
		err := s.WithTenant(ctx, uuid.New(), func(ctx context.Context, q store.Querier) error {
			// Empty name violates the CHECK constraint on projects.name.
			_, cerr := q.CreateProject(ctx, store.CreateProjectParams{Name: ""})

			return cerr
		})
		if err == nil {
			t.Fatal("WithTenant returned nil, want the constraint violation")
		}

		t.Logf("statement error surfaced as: %v", err)

		assertPoolIdle(t, pool, "after a statement error")
		assertNoNewConns(t, pool, baseline, "after a statement error")
	})

	t.Run("cancelled context", func(t *testing.T) {
		// The request-was-abandoned case. The rollback deliberately does not
		// inherit the caller's context, because one that inherits a cancelled
		// context fails instantly and the connection is destroyed instead of
		// returned — which is exactly the moment a service under load can least
		// afford pool churn.
		cancelCtx, cancel := context.WithCancel(ctx)

		err := s.WithTenant(cancelCtx, uuid.New(), func(ctx context.Context, q store.Querier) error {
			cancel()

			return ctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WithTenant error = %v, want context.Canceled", err)
		}

		assertPoolIdle(t, pool, "after a cancelled context")
		assertNoNewConns(t, pool, baseline, "after a cancelled context")
	})

	t.Run("panic in the callback", func(t *testing.T) {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic did not propagate out of WithTenant")
				}
			}()

			_ = s.WithTenant(ctx, uuid.New(), func(ctx context.Context, q store.Querier) error {
				panic("callback exploded")
			})
		}()

		assertPoolIdle(t, pool, "after a panic")
		assertNoNewConns(t, pool, baseline, "after a panic")
	})
}

// TestCallbackErrorRollsBackTheTransaction proves the rollback is real work and
// not just a deferred call that happens to return nil.
func TestCallbackErrorRollsBackTheTransaction(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 2)
	superuser := newSuperuserPool(t)
	a, _, _ := seedTenants(t, superuser)
	s := store.New(pool)

	const name = "rolled back project"

	err := s.WithTenant(ctx, a.TenantID, func(ctx context.Context, q store.Querier) error {
		created, cerr := q.CreateProject(ctx, store.CreateProjectParams{Name: name})
		if cerr != nil {
			return cerr
		}

		// The insert took the tenant from the transaction, not from an
		// argument, so this is also the proof that current_tenant_id() is what
		// lands in the row.
		if created.TenantID != a.TenantID {
			t.Errorf("created project tenant = %s, want %s", created.TenantID, a.TenantID)
		}

		t.Logf("created project %s in tenant %s, then failing on purpose", created.ID, created.TenantID)

		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("WithTenant error = %v, want %v", err, errBoom)
	}

	names, err := store.InTenant(ctx, s, a.TenantID, func(ctx context.Context, q store.Querier) ([]string, error) {
		projects, lerr := q.ListProjects(ctx)

		return projectNames(projects), lerr
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}

	t.Logf("after rollback, tenant %s sees: %v", a.Label, names)

	for _, got := range names {
		if got == name {
			t.Fatalf("%q survived a rolled-back transaction", name)
		}
	}
}

// TestInTenantCommitsAndReturns is the happy path for the value-returning
// wrapper, including that its work actually persists.
func TestInTenantCommitsAndReturns(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 2)
	superuser := newSuperuserPool(t)
	a, b, _ := seedTenants(t, superuser)
	s := store.New(pool)

	created, err := store.InTenant(ctx, s, a.TenantID, func(ctx context.Context, q store.Querier) (store.Project, error) {
		return q.CreateProject(ctx, store.CreateProjectParams{Name: "committed project"})
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}

	t.Logf("committed project %s (tenant %s)", created.ID, created.TenantID)

	seenByA, err := store.InTenant(ctx, s, a.TenantID, func(ctx context.Context, q store.Querier) (int, error) {
		projects, lerr := q.ListProjects(ctx)

		return len(projects), lerr
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}

	seenByB, err := store.InTenant(ctx, s, b.TenantID, func(ctx context.Context, q store.Querier) (int, error) {
		projects, lerr := q.ListProjects(ctx)

		return len(projects), lerr
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}

	t.Logf("projects visible after the commit: tenant %s = %d, tenant %s = %d", a.Label, seenByA, b.Label, seenByB)

	if seenByA != 2 {
		t.Errorf("tenant %s sees %d projects, want 2", a.Label, seenByA)
	}

	if seenByB != 1 {
		t.Errorf("tenant %s sees %d projects, want 1 (its own only)", b.Label, seenByB)
	}
}

func assertPoolIdle(t *testing.T, pool *pgxpool.Pool, when string) {
	t.Helper()

	stat := pool.Stat()

	t.Logf("%s: total=%d acquired=%d idle=%d new_conns=%d",
		when, stat.TotalConns(), stat.AcquiredConns(), stat.IdleConns(), stat.NewConnsCount())

	if stat.AcquiredConns() != 0 {
		t.Errorf("%s: %d connections still acquired, want 0", when, stat.AcquiredConns())
	}

	if stat.TotalConns() != 1 || stat.IdleConns() != 1 {
		t.Errorf("%s: total=%d idle=%d, want 1 idle connection back in the pool",
			when, stat.TotalConns(), stat.IdleConns())
	}
}

func assertNoNewConns(t *testing.T, pool *pgxpool.Pool, baseline int64, when string) {
	t.Helper()

	if got := pool.Stat().NewConnsCount(); got != baseline {
		t.Errorf("%s: NewConnsCount = %d, want %d — the pool had to build a replacement, so the original was destroyed rather than returned",
			when, got, baseline)
	}
}

func backendPID(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var pid int

	if err := pool.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("reading backend pid: %v", err)
	}

	return pid
}

func projectNames(projects []store.Project) []string {
	names := make([]string, 0, len(projects))
	for _, p := range projects {
		names = append(names, p.Name)
	}

	sort.Strings(names)

	return names
}

func memberEmails(members []store.ListMembersRow) []string {
	emails := make([]string, 0, len(members))
	for _, m := range members {
		emails = append(emails, m.Email)
	}

	sort.Strings(emails)

	return emails
}
