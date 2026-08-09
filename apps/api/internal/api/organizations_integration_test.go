//go:build integration

package api_test

// POST /api/v1/organizations end to end, against a real Postgres and a real
// Redis (issue #34).
//
// # Why this file exists rather than another unit test
//
// The acceptance criterion is that an integration test drives the partial state
// *deliberately* — fails registration's second transaction — and then recovers
// from it. The weaker version of this test would insert a `users` row through
// the owner pool and call that a half-registered account. It would pass against
// code that cannot actually recover the real thing, because it would be
// recovering from a state the test invented rather than from the one
// [auth.Service.Register] produces.
//
// So the failure is injected at the real seam. brokenTenantDoor wraps the real
// store and, while armed, runs the second transaction for real against Postgres
// — CreateOrganization and CreateMembership both execute — and then returns an
// error from the callback, which is exactly how internal/store rolls a
// transaction back. Register's own code path is untouched: it calls WithTenant,
// WithTenant fails, and it logs auth.register.partial and returns. What is left
// behind is a committed user and credential from the first transaction and
// nothing from the second, which is the production failure mode reproduced
// rather than modelled.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// brokenTenantDoor is the real store with a switch on its tenant-scoped door.
//
// WithoutTenant is promoted from the embedded *store.Store and is never
// interfered with — the first of registration's two transactions has to commit
// for real, or there is no partial state to recover from.
type brokenTenantDoor struct {
	*store.Store

	armed atomic.Bool
}

// errSecondTransactionLost stands in for the connection drop, pod eviction or
// failover that this window is actually exposed to.
var errSecondTransactionLost = &lostConnection{}

type lostConnection struct{}

func (*lostConnection) Error() string { return "connection reset by peer" }

func (b *brokenTenantDoor) WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error {
	if !b.armed.Load() {
		return b.Store.WithTenant(ctx, tenantID, fn)
	}

	// The statements run, and then the transaction does not commit. Postgres
	// performs the rollback, not this fake.
	err := b.Store.WithTenant(ctx, tenantID, func(ctx context.Context, q store.Querier) error {
		if ferr := fn(ctx, q); ferr != nil {
			return ferr
		}

		return errSecondTransactionLost
	})
	if err == nil {
		return errSecondTransactionLost
	}

	return err
}

// TestARegistrationThatFailedHalfwayRecoversWithoutAnOperator is issue #34's
// acceptance criteria 1, 3 and 4, in order, against a real database.
func TestARegistrationThatFailedHalfwayRecoversWithoutAnOperator(t *testing.T) {
	var breaker *brokenTenantDoor

	s := newServer(t, generousLimits(), func(real *store.Store) auth.Store {
		breaker = &brokenTenantDoor{Store: real}

		return breaker
	})

	email := "stranded-" + uuid.NewString()[:8] + "@example.com"

	// Criterion 3: the partial state is driven deliberately, through the real
	// registration endpoint.
	breaker.armed.Store(true)

	failed := s.do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email":             email,
		"password":          integrationPass,
		"display_name":      "Stranded Person",
		"organization_name": "Lost Workspace",
	})
	if failed.status != http.StatusInternalServerError {
		t.Fatalf("register with a failing second transaction: status %d, body %s", failed.status, failed.raw)
	}

	breaker.armed.Store(false)

	// Criterion 4: auth.register.partial is the only signal an operator gets,
	// and it carries the user id. Reading it back is also how this test learns
	// which account was stranded — the response could not say, because there
	// was no successful response.
	userID := partialRegistrationUserID(t, s.logs.String())

	t.Logf("registration left user %s (%s) with no organization", userID, email)

	t.Cleanup(func() {
		owner := testDB.OwnerPool(t, 2)

		if _, err := owner.Exec(context.Background(),
			`DELETE FROM organizations WHERE id IN (SELECT tenant_id FROM memberships WHERE user_id = $1)`,
			userID); err != nil {
			t.Errorf("cleaning up the recovered organization: %v", err)
		}

		if _, err := owner.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleaning up user %s: %v", userID, err)
		}
	})

	// The state really is the one the issue describes: the account exists and
	// authenticates, and the second transaction left nothing behind.
	if n := membershipCount(t, userID); n != 0 {
		t.Fatalf("the failed registration left %d membership(s); the rollback did not happen, "+
			"so this test is not recovering from the state it claims", n)
	}

	blocked := s.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email": email, "password": integrationPass,
	})
	if blocked.status != http.StatusForbidden {
		t.Fatalf("login for a half-registered account: status %d, body %s", blocked.status, blocked.raw)
	}

	t.Logf("login before recovery -> %d %s", blocked.status, blocked.raw)

	// Criterion 1: the account reaches a working state on its own.
	created := s.do(t, http.MethodPost, "/api/v1/organizations", "", map[string]string{
		"email":             email,
		"password":          integrationPass,
		"organization_name": "Second Time Lucky",
	})
	if created.status != http.StatusCreated {
		t.Fatalf("creating the missing organization: status %d, body %s", created.status, created.raw)
	}

	if got := stringField(t, created.body, "user_id"); got != userID.String() {
		t.Errorf("the organization was created for user %s, want the stranded account %s", got, userID)
	}

	organization, ok := created.body["organization"].(map[string]any)
	if !ok {
		t.Fatalf("no organization in the response: %s", created.raw)
	}

	tenantID := uuid.MustParse(stringField(t, organization, "id"))

	if role := stringField(t, organization, "role"); role != auth.RoleOwner {
		t.Errorf("role = %q, want %q — the account that creates an organization owns it", role, auth.RoleOwner)
	}

	// Criterion 2, the database half: the membership landed on the calling
	// subject, in the organization just created, as owner. Read through the
	// owner pool rather than through the API, so the assertion does not depend
	// on the code path it is checking.
	assertOwnerMembership(t, userID, tenantID)

	// "Working state" means working, not merely present.
	session := s.login(t, email, integrationPass)

	me := s.do(t, http.MethodGet, "/api/v1/me", session.accessToken, nil)
	if me.status != http.StatusOK {
		t.Fatalf("GET /me after recovery: status %d, body %s", me.status, me.raw)
	}

	current, ok := me.body["organization"].(map[string]any)
	if !ok {
		t.Fatalf("GET /me has no current organization: %s", me.raw)
	}

	if got := stringField(t, current, "id"); got != tenantID.String() {
		t.Errorf("GET /me names organization %s, want the recovered one %s", got, tenantID)
	}

	t.Logf("recovered: %s now owns %s and /me agrees", email, tenantID)

	// And the repair is not repeatable — a second call is a conflict, not a
	// second workspace.
	again := s.do(t, http.MethodPost, "/api/v1/organizations", "", map[string]string{
		"email": email, "password": integrationPass, "organization_name": "Third Time",
	})
	if again.status != http.StatusConflict {
		t.Errorf("a second creation: status %d, want %d — body %s", again.status, http.StatusConflict, again.raw)
	}

	if n := membershipCount(t, userID); n != 1 {
		t.Errorf("the account holds %d memberships after a refused second attempt, want 1", n)
	}
}

// TestCreatingAnOrganizationCannotBeAimedAtAnotherAccount is criterion 2.
//
// The endpoint takes no user id, so the interesting attacks are the two that
// remain: name the victim's address without their password, and supply the
// victim's id alongside credentials that do verify. Neither may move the
// organization off the account whose password was checked.
func TestCreatingAnOrganizationCannotBeAimedAtAnotherAccount(t *testing.T) {
	var breaker *brokenTenantDoor

	s := newServer(t, generousLimits(), func(real *store.Store) auth.Store {
		breaker = &brokenTenantDoor{Store: real}

		return breaker
	})

	victim := s.strand(t, breaker, "victim")
	attacker := s.strand(t, breaker, "attacker")

	// 1. The victim's address, without the victim's password.
	guessed := s.do(t, http.MethodPost, "/api/v1/organizations", "", map[string]string{
		"email": victim.email, "password": "not the right password at all",
	})
	if guessed.status != http.StatusUnauthorized {
		t.Errorf("naming the victim's address with a wrong password: status %d, want %d — body %s",
			guessed.status, http.StatusUnauthorized, guessed.raw)
	}

	// The same answer an address with no account gets, so this is not an
	// existence oracle either.
	unknown := s.do(t, http.MethodPost, "/api/v1/organizations", "", map[string]string{
		"email": "nobody-" + uuid.NewString()[:8] + "@example.com", "password": integrationPass,
	})
	if unknown.status != guessed.status || unknown.raw != guessed.raw {
		t.Errorf("an unknown address answers %d %s but a wrong password answers %d %s; "+
			"the difference tells an attacker which addresses are registered",
			unknown.status, unknown.raw, guessed.status, guessed.raw)
	}

	if n := membershipCount(t, victim.userID); n != 0 {
		t.Fatalf("the victim gained %d membership(s) from a refused request", n)
	}

	// 2. The attacker's own credentials, with the victim's id smuggled into
	// every field name a body could plausibly carry one in.
	steered := s.do(t, http.MethodPost, "/api/v1/organizations", "", map[string]any{
		"email":             attacker.email,
		"password":          integrationPass,
		"organization_name": "Steered",
		"user_id":           victim.userID.String(),
		"subject":           victim.userID.String(),
		"owner_id":          victim.userID.String(),
		"sub":               victim.userID.String(),
	})
	if steered.status != http.StatusCreated {
		t.Fatalf("the attacker's own creation: status %d, body %s", steered.status, steered.raw)
	}

	if got := stringField(t, steered.body, "user_id"); got != attacker.userID.String() {
		t.Errorf("the organization was created for %s; the request named the victim %s and the "+
			"caller was %s", got, victim.userID, attacker.userID)
	}

	if n := membershipCount(t, victim.userID); n != 0 {
		t.Errorf("the victim gained %d membership(s) from the attacker's request", n)
	}

	t.Logf("victim %s still holds no membership; attacker %s holds %d",
		victim.userID, attacker.userID, membershipCount(t, attacker.userID))
}

// strand registers an account whose second transaction fails, and returns it.
func (s *server) strand(t *testing.T, breaker *brokenTenantDoor, label string) account {
	t.Helper()

	email := label + "-" + uuid.NewString()[:8] + "@example.com"

	breaker.armed.Store(true)

	before := s.logs.Len()

	failed := s.do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email":        email,
		"password":     integrationPass,
		"display_name": label + " person",
	})

	breaker.armed.Store(false)

	if failed.status != http.StatusInternalServerError {
		t.Fatalf("register with a failing second transaction: status %d, body %s", failed.status, failed.raw)
	}

	userID := partialRegistrationUserID(t, s.logs.String()[before:])

	t.Cleanup(func() {
		owner := testDB.OwnerPool(t, 2)

		if _, err := owner.Exec(context.Background(),
			`DELETE FROM organizations WHERE id IN (SELECT tenant_id FROM memberships WHERE user_id = $1)`,
			userID); err != nil {
			t.Errorf("cleaning up organizations for %s: %v", userID, err)
		}

		if _, err := owner.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleaning up user %s: %v", userID, err)
		}
	})

	return account{email: email, userID: userID}
}

// partialRegistrationUserID finds the auth.register.partial line and returns the
// user id it names, failing the test when there is none.
//
// Parsing the log rather than asserting a substring is deliberate: the criterion
// is that an operator can identify *which* account was stranded, and a line that
// said only "registration failed" would satisfy a substring check while being
// useless.
func partialRegistrationUserID(t *testing.T, logs string) uuid.UUID {
	t.Helper()

	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, "auth.register.partial") {
			continue
		}

		var entry struct {
			Event  string `json:"event"`
			UserID string `json:"user_id"`
		}

		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		if entry.Event != "auth.register.partial" {
			continue
		}

		if entry.UserID == "" {
			t.Fatalf("auth.register.partial carries no user_id, so an operator cannot tell which "+
				"account was left without an organization: %s", line)
		}

		return uuid.MustParse(entry.UserID)
	}

	t.Fatalf("no auth.register.partial in the logs; the one signal this failure produces is missing:\n%s", logs)

	return uuid.Nil
}

// membershipCount reads straight from the database as the owner, bypassing both
// the API and row-level security, so "the victim has nothing" is a fact about
// the table rather than about what one tenant can see.
func membershipCount(t *testing.T, userID uuid.UUID) int {
	t.Helper()

	var count int

	if err := testDB.OwnerPool(t, 2).QueryRow(context.Background(),
		`SELECT count(*) FROM memberships WHERE user_id = $1`, userID).Scan(&count); err != nil {
		t.Fatalf("counting memberships for %s: %v", userID, err)
	}

	return count
}

func assertOwnerMembership(t *testing.T, userID, tenantID uuid.UUID) {
	t.Helper()

	var role string

	if err := testDB.OwnerPool(t, 2).QueryRow(context.Background(),
		`SELECT role FROM memberships WHERE user_id = $1 AND tenant_id = $2`,
		userID, tenantID).Scan(&role); err != nil {
		t.Fatalf("the recovered account holds no membership in %s: %v", tenantID, err)
	}

	if role != auth.RoleOwner {
		t.Errorf("membership role = %q, want %q", role, auth.RoleOwner)
	}
}
