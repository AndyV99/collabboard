//go:build integration

package store_test

// The pre-tenant identity path against a real Postgres, as the serving role.
//
// Every test in this file runs through store.WithoutTenant on a pool connected
// as collabboard_app — the role the API actually serves with, which is not a
// superuser, holds no BYPASSRLS and owns nothing. identity_test.go asserts that
// from inside the connection. Run as anyone else these assertions would pass
// without the SECURITY DEFINER functions existing at all.
//
// The claim each test makes is "this operation is possible, and it is possible
// *only* through this door". The second half lives in pretenant_narrow_test.go.

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// uniqueViolation is the SQLSTATE for a duplicate key. Asserted specifically,
// because a CHECK failure would also make the insert fail and would say nothing
// about email uniqueness.
const uniqueViolation = "23505"

// TestLoginByEmailWorksWithoutATenant is acceptance criterion 1: an account can
// be found by email with no tenant context and without the caller holding
// BYPASSRLS or connecting as the schema owner.
//
// The control matters as much as the assertion. The same pool, in the same
// process, sees zero rows in users when it asks directly — so the lookup
// succeeding proves the SECURITY DEFINER path, not a missing policy.
func TestLoginByEmailWorksWithoutATenant(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 2)
	owner := newOwnerPool(t)
	a, _, _ := seedTenants(t, owner)
	s := store.New(pool)

	var directlyVisible int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&directlyVisible); err != nil {
		t.Fatalf("counting users directly: %v", err)
	}

	t.Logf("the same pool, asking users directly with no tenant: %d rows", directlyVisible)

	if directlyVisible != 0 {
		t.Fatalf("the app role can read %d users without a tenant; the rest of this test proves nothing about the identity path",
			directlyVisible)
	}

	found, err := store.WithoutTenantValue(ctx, s, store.ReasonLogin,
		func(ctx context.Context, q store.IdentityQuerier) (store.IdentityUser, error) {
			return q.FindUserByEmail(ctx, a.MemberEmail)
		})
	if err != nil {
		t.Fatalf("FindUserByEmail(%s): %v", a.MemberEmail, err)
	}

	t.Logf("login lookup for %s -> id=%s display_name=%q", a.MemberEmail, found.ID, found.DisplayName)

	if found.ID != a.MemberID {
		t.Errorf("FindUserByEmail(%s).ID = %s, want %s", a.MemberEmail, found.ID, a.MemberID)
	}
}

// TestLoginByEmailIsCaseInsensitive matches users_email_key, which is UNIQUE on
// lower(email). If the lookup were case-sensitive, an address could be
// unregistrable and unloggable-in at the same time: the index would reject the
// signup and the query would not find the existing row.
func TestLoginByEmailIsCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	a, _, _ := seedTenants(t, owner)
	s := store.New(newPool(t, 2))

	for _, spelling := range []string{
		strings.ToUpper(a.MemberEmail),
		"  " + a.MemberEmail + "  ",
	} {
		found, err := store.WithoutTenantValue(ctx, s, store.ReasonLogin,
			func(ctx context.Context, q store.IdentityQuerier) (store.IdentityUser, error) {
				return q.FindUserByEmail(ctx, spelling)
			})
		if err != nil {
			t.Errorf("FindUserByEmail(%q): %v", spelling, err)

			continue
		}

		t.Logf("FindUserByEmail(%q) -> %s", spelling, found.ID)

		if found.ID != a.MemberID {
			t.Errorf("FindUserByEmail(%q).ID = %s, want %s", spelling, found.ID, a.MemberID)
		}
	}
}

// TestLoginByEmailReportsAnUnknownAddressAsNoRow pins the not-found signal,
// because #8 has to branch on it and a zero-value user would be worse than an
// error.
func TestLoginByEmailReportsAnUnknownAddressAsNoRow(t *testing.T) {
	ctx := context.Background()
	s := store.New(newPool(t, 1))

	_, err := store.WithoutTenantValue(ctx, s, store.ReasonLogin,
		func(ctx context.Context, q store.IdentityQuerier) (store.IdentityUser, error) {
			return q.FindUserByEmail(ctx, "nobody-"+uuid.NewString()+"@example.com")
		})

	t.Logf("FindUserByEmail(unregistered) -> %v", err)

	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("FindUserByEmail(unregistered) = %v, want store.ErrNotFound", err)
	}
}

// TestListUserOrganizationsSpansTenants is acceptance criterion 2, and the test
// the two-tenant fixture exists for.
//
// The shared user belongs to both organizations. No tenant-scoped transaction
// could return both rows — scoped to A it would see A's membership only, and
// scoped to B, B's — so a result containing both is only reachable through the
// pre-tenant path.
func TestListUserOrganizationsSpansTenants(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	fixture := seedFixture(t, owner)
	s := store.New(newPool(t, 2))

	orgs, err := store.WithoutTenantValue(ctx, s, store.ReasonListOrganizations,
		func(ctx context.Context, q store.IdentityQuerier) ([]store.UserOrganization, error) {
			return q.ListUserOrganizations(ctx, fixture.SharedUserID)
		})
	if err != nil {
		t.Fatalf("ListUserOrganizations: %v", err)
	}

	got := make([]uuid.UUID, 0, len(orgs))
	for _, o := range orgs {
		got = append(got, o.OrganizationID)

		t.Logf("shared user belongs to %s (%s) as %s", o.Name, o.OrganizationID, o.Role)
	}

	for _, want := range []tenantFixture{fixture.A, fixture.B} {
		if !slices.Contains(got, want.TenantID) {
			t.Errorf("ListUserOrganizations is missing %s (%s); the pre-tenant path is not spanning tenants",
				want.Label, want.TenantID)
		}
	}

	if len(orgs) != 2 {
		t.Errorf("ListUserOrganizations returned %d organizations, want exactly 2", len(orgs))
	}

	// The user who belongs to one organization gets exactly one back, so the
	// query is filtering by user rather than returning every membership.
	solo, err := store.WithoutTenantValue(ctx, s, store.ReasonListOrganizations,
		func(ctx context.Context, q store.IdentityQuerier) ([]store.UserOrganization, error) {
			return q.ListUserOrganizations(ctx, fixture.A.MemberID)
		})
	if err != nil {
		t.Fatalf("ListUserOrganizations(single-org user): %v", err)
	}

	t.Logf("%s's own member belongs to %d organization(s)", fixture.A.Label, len(solo))

	if len(solo) != 1 || solo[0].OrganizationID != fixture.A.TenantID {
		t.Errorf("ListUserOrganizations(%s's member) = %v, want exactly %s", fixture.A.Label, solo, fixture.A.TenantID)
	}
}

// TestInvitingAnExistingUserFromAnotherOrganization is acceptance criterion 3,
// end to end and in the order a handler would do it.
//
// It is deliberately split across both doors, because that split is the design:
// resolving the address is pre-tenant, and creating the membership is not.
func TestInvitingAnExistingUserFromAnotherOrganization(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	a, b, _ := seedTenants(t, owner)
	s := store.New(newPool(t, 3))

	// Before: b's member is invisible to a, which is what makes the invite hard
	// in the first place.
	before := memberEmailsIn(ctx, t, s, a)
	t.Logf("%s's members before the invite: %v", a.Label, before)

	if slices.Contains(before, b.MemberEmail) {
		t.Fatalf("%s can already see %s; the fixture is wrong", a.Label, b.MemberEmail)
	}

	// Step one, pre-tenant: turn the typed address into a user id. This is the
	// step no tenant-scoped transaction can do.
	userID, err := store.WithoutTenantValue(ctx, s, store.ReasonInviteLookup,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.ResolveUserIDByEmail(ctx, b.MemberEmail)
		})
	if err != nil {
		t.Fatalf("ResolveUserIDByEmail(%s): %v", b.MemberEmail, err)
	}

	t.Logf("%s resolved %s to user %s — and learned nothing else about %s", a.Label, b.MemberEmail, userID, b.Label)

	if userID != b.MemberID {
		t.Fatalf("ResolveUserIDByEmail(%s) = %s, want %s", b.MemberEmail, userID, b.MemberID)
	}

	// Step two, tenant-scoped: join them to *this* organization. The tenant
	// comes from the transaction, so there is no argument an admin could use to
	// add a member to someone else's org.
	membership, err := store.InTenant(ctx, s, a.TenantID,
		func(ctx context.Context, q store.Querier) (store.Membership, error) {
			return q.CreateMembership(ctx, store.CreateMembershipParams{UserID: userID, Role: "member"})
		})
	if err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}

	t.Logf("created membership %s in tenant %s", membership.ID, membership.TenantID)

	if membership.TenantID != a.TenantID {
		t.Errorf("membership tenant = %s, want %s", membership.TenantID, a.TenantID)
	}

	// After: the invited user is visible to a, because users_visible_via_membership
	// is derived from the row that was just created.
	after := memberEmailsIn(ctx, t, s, a)
	t.Logf("%s's members after the invite: %v", a.Label, after)

	if !slices.Contains(after, b.MemberEmail) {
		t.Errorf("%s still cannot see %s after inviting them", a.Label, b.MemberEmail)
	}

	// And b is unchanged: an invite adds a membership, it does not move anyone.
	inB := memberEmailsIn(ctx, t, s, b)
	t.Logf("%s's members are unchanged: %v", b.Label, inB)

	if slices.Contains(inB, a.MemberEmail) {
		t.Errorf("%s can now see %s; the invite leaked in the wrong direction", b.Label, a.MemberEmail)
	}
}

// TestResolveUserIDDiscloseNothingButTheID is the "does not reveal anything
// about that other organization" half of acceptance criterion 3, asserted on
// the shape of the answer rather than on a promise in a comment.
//
// ResolveUserIDByEmail returns a bare uuid: there is no display name to leak,
// no organization list, and no created_at to correlate on. Anything more would
// have to be added to the function in migration 00004 and would show up in the
// generated signature this test reads.
func TestResolveUserIDDiscloseNothingButTheID(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	_, b, _ := seedTenants(t, owner)
	s := store.New(newPool(t, 1))

	id, err := store.WithoutTenantValue(ctx, s, store.ReasonInviteLookup,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.ResolveUserIDByEmail(ctx, b.MemberEmail)
		})
	if err != nil {
		t.Fatalf("ResolveUserIDByEmail: %v", err)
	}

	// uuid.UUID is [16]byte. If the query ever grew a struct return, this stops
	// compiling — which is the notification this test exists to give.
	t.Logf("the invite lookup returned %T (%s) and nothing else", id, id)

	if id != b.MemberID {
		t.Errorf("ResolveUserIDByEmail = %s, want %s", id, b.MemberID)
	}
}

// TestResolveUserIDReportsAnUnregisteredAddressAsNoRow is the branch that sends
// an invite down the "create the user first" path.
func TestResolveUserIDReportsAnUnregisteredAddressAsNoRow(t *testing.T) {
	ctx := context.Background()
	s := store.New(newPool(t, 1))

	_, err := store.WithoutTenantValue(ctx, s, store.ReasonInviteLookup,
		func(ctx context.Context, q store.IdentityQuerier) (uuid.UUID, error) {
			return q.ResolveUserIDByEmail(ctx, "unregistered-"+uuid.NewString()+"@example.com")
		})

	t.Logf("ResolveUserIDByEmail(unregistered) -> %v", err)

	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ResolveUserIDByEmail(unregistered) = %v, want store.ErrNotFound", err)
	}
}

// TestCreatingAUserIsOnlyPossibleThroughThePreTenantPath covers registration and
// the "invited address has no account yet" branch, and pairs it with the proof
// that the tenant-scoped door cannot do the same thing.
func TestCreatingAUserIsOnlyPossibleThroughThePreTenantPath(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	a, _, _ := seedTenants(t, owner)
	tenantPool := newPool(t, 2)
	s := store.New(tenantPool)

	email := "registered-" + uuid.NewString()[:8] + "@example.com"

	created, err := store.WithoutTenantValue(ctx, s, store.ReasonRegisterUser,
		func(ctx context.Context, q store.IdentityQuerier) (store.CreatedUser, error) {
			return q.CreateUser(ctx, store.CreateUserParams{Email: email, DisplayName: "Registered Person"})
		})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	t.Cleanup(func() {
		if _, derr := owner.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, created.ID); derr != nil {
			t.Errorf("cleaning up the registered user: %v", derr)
		}
	})

	t.Logf("registered %s as %s", created.Email, created.ID)

	if created.Email != email {
		t.Errorf("CreateUser stored email %q, want %q", created.Email, email)
	}

	// The same insert from inside a tenant is refused by the WITH CHECK half of
	// users_visible_via_membership, which is the reason this operation needs a
	// door of its own rather than a convenience wrapper. Raw SQL, because the
	// tenant-scoped querier has no CreateUser to call — which is itself part of
	// the point.
	inTenantTx(t, tenantPool, a.TenantID, func(tx pgx.Tx) {
		_, ierr := tx.Exec(ctx, `INSERT INTO users (email, display_name) VALUES ($1, $2)`,
			"blocked-"+uuid.NewString()[:8]+"@example.com", "Blocked")

		t.Logf("the same insert from inside tenant %s -> %v", a.Label, ierr)

		if ierr == nil {
			t.Fatal("a tenant-scoped transaction created a user; the pre-tenant path would not be necessary")
		}

		var pgErr *pgconn.PgError
		if !errors.As(ierr, &pgErr) {
			t.Fatalf("the tenant-scoped insert failed with a non-Postgres error: %v", ierr)
		}

		if pgErr.Code != insufficientPrivilege {
			t.Errorf("the tenant-scoped insert was rejected with SQLSTATE %s (%s), want %s — something other than the policy stopped it",
				pgErr.Code, pgErr.Message, insufficientPrivilege)
		}
	})
}

// TestRegisteringADuplicateAddressIsRejected keeps "already registered" and
// "here is your new account" distinguishable. Without this, an invite flow that
// skipped the resolve step would silently hand back somebody else's row.
func TestRegisteringADuplicateAddressIsRejected(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	a, _, _ := seedTenants(t, owner)
	s := store.New(newPool(t, 1))

	_, err := store.WithoutTenantValue(ctx, s, store.ReasonRegisterUser,
		func(ctx context.Context, q store.IdentityQuerier) (store.CreatedUser, error) {
			return q.CreateUser(ctx, store.CreateUserParams{Email: a.MemberEmail, DisplayName: "Impostor"})
		})
	if err == nil {
		t.Fatal("CreateUser accepted an address that is already registered")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("CreateUser failed with a non-Postgres error: %v", err)
	}

	t.Logf("re-registering %s -> SQLSTATE %s (%s)", a.MemberEmail, pgErr.Code, pgErr.ConstraintName)

	if pgErr.Code != uniqueViolation {
		t.Errorf("CreateUser rejected the duplicate with SQLSTATE %s, want %s — something other than the email index stopped it",
			pgErr.Code, uniqueViolation)
	}
}

// TestWithoutTenantRunsWithNoTenantContextAndLeavesNone is the transaction-level
// claim, on a pool of exactly one connection so the before and after provably
// describe the same Postgres backend.
//
// Two things at once: the pre-tenant transaction really has no tenant (so a
// leftover GUC cannot make an identity lookup behave differently depending on
// which request ran before it), and it leaves none behind (so the next request
// through this connection starts clean).
func TestWithoutTenantRunsWithNoTenantContextAndLeavesNone(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t, 1)
	owner := newOwnerPool(t)
	a, _, _ := seedTenants(t, owner)
	s := store.New(pool)

	// Deliberately dirty the connection first: a committed tenant transaction
	// on the same backend, so "no tenant context" is a thing WithoutTenant did
	// rather than a thing that was already true.
	if err := s.WithTenant(ctx, a.TenantID, func(ctx context.Context, q store.Querier) error {
		_, lerr := q.ListProjects(ctx)

		return lerr
	}); err != nil {
		t.Fatalf("WithTenant: %v", err)
	}

	before := backendPID(t, pool)

	if err := s.WithoutTenant(ctx, store.ReasonLogin, func(ctx context.Context, q store.IdentityQuerier) error {
		_, ferr := q.FindUserByEmail(ctx, a.MemberEmail)

		return ferr
	}); err != nil {
		t.Fatalf("WithoutTenant: %v", err)
	}

	var (
		after     int
		tenantGUC string
		projects  int
	)

	err := pool.QueryRow(ctx, `
		SELECT pg_backend_pid(),
		       coalesce(current_setting('app.tenant_id', true), ''),
		       (SELECT count(*) FROM projects)
	`).Scan(&after, &tenantGUC, &projects)
	if err != nil {
		t.Fatalf("querying after WithoutTenant: %v", err)
	}

	t.Logf("after WithoutTenant: backend pid %d (was %d), app.tenant_id=%q, projects visible: %d",
		after, before, tenantGUC, projects)

	if after != before {
		t.Fatalf("backend pid changed (%d -> %d); the pool handed out a different connection, so this proves nothing",
			before, after)
	}

	if tenantGUC != "" {
		t.Errorf("app.tenant_id = %q after WithoutTenant, want empty", tenantGUC)
	}

	if projects != 0 {
		t.Errorf("a query after WithoutTenant saw %d projects, want 0", projects)
	}
}

// TestWithoutTenantRefusesTheZeroReason asserts the check the unit tests cannot:
// with a real pool behind it, the zero IdentityReason is the only thing left to
// fail on, so the sentinel is unambiguous.
func TestWithoutTenantRefusesTheZeroReason(t *testing.T) {
	s := store.New(newPool(t, 1))

	called := false

	err := s.WithoutTenant(context.Background(), store.IdentityReason{},
		func(context.Context, store.IdentityQuerier) error {
			called = true

			return nil
		})

	t.Logf("WithoutTenant with a zero reason -> %v", err)

	if !errors.Is(err, store.ErrNoIdentityReason) {
		t.Errorf("WithoutTenant = %v, want store.ErrNoIdentityReason", err)
	}

	if called {
		t.Error("the callback ran despite the blank reason")
	}
}

// TestWithoutTenantRollsBackOnCallbackError proves the pre-tenant transaction is
// a real transaction. Registration is a write, so "the callback failed but the
// user was created anyway" is a state a signup flow could get stuck in.
func TestWithoutTenantRollsBackOnCallbackError(t *testing.T) {
	ctx := context.Background()
	owner := newOwnerPool(t)
	s := store.New(newPool(t, 2))

	email := "rolled-back-" + uuid.NewString()[:8] + "@example.com"

	err := s.WithoutTenant(ctx, store.ReasonRegisterUser, func(ctx context.Context, q store.IdentityQuerier) error {
		created, cerr := q.CreateUser(ctx, store.CreateUserParams{Email: email, DisplayName: "Doomed"})
		if cerr != nil {
			return cerr
		}

		t.Logf("created %s inside the transaction, then failing on purpose", created.ID)

		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("WithoutTenant error = %v, want %v", err, errBoom)
	}

	// Asked through the owner pool, which is subject to no policy at all — so a
	// surviving row would be found here even though the app role could not see
	// it.
	var survived int
	if serr := owner.QueryRow(ctx, `SELECT count(*) FROM users WHERE email = $1`, email).Scan(&survived); serr != nil {
		t.Fatalf("counting the rolled-back user: %v", serr)
	}

	t.Logf("after rollback, %s exists in %d rows", email, survived)

	if survived != 0 {
		t.Errorf("%q survived a rolled-back pre-tenant transaction", email)
	}
}

// memberEmailsIn lists the emails a tenant can see through the tenant-scoped
// door, which is the view an invite is supposed to change.
func memberEmailsIn(ctx context.Context, t *testing.T, s *store.Store, tenant tenantFixture) []string {
	t.Helper()

	members, err := store.InTenant(ctx, s, tenant.TenantID,
		func(ctx context.Context, q store.Querier) ([]store.ListMembersRow, error) {
			return q.ListMembers(ctx)
		})
	if err != nil {
		t.Fatalf("ListMembers in %s: %v", tenant.Label, err)
	}

	emails := make([]string, 0, len(members))
	for _, m := range members {
		emails = append(emails, m.Email)
	}

	sort.Strings(emails)

	return emails
}
