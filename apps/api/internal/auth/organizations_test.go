package auth_test

// CreateFirstOrganization, against fakes (issue #34).
//
// The test that carries the weight is
// TestAnAccountStrandedByAFailedRegistrationCanRecoverItself: it drives the real
// Register through a store whose *tenant-scoped door only* fails, which is the
// production failure mode rather than an invented one, and then recovers from
// the state that produced. The end-to-end version against real Postgres is
// internal/api/organizations_integration_test.go.
//
// The rest are the refusals, and they matter because this endpoint takes a
// password from an anonymous caller: it has to be no weaker than login about
// what it discloses, and no cheaper to attack.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// errSecondTransactionLost stands in for what actually happens: a connection
// drop, a pod eviction or a failover between registration's two transactions.
var errSecondTransactionLost = errors.New("connection reset by peer")

// strand runs a registration whose second transaction fails, and returns the
// address left holding an account with no organization.
//
// It goes through the real [auth.Service.Register] rather than seeding a user
// row directly. That distinction is the point of the test: seeding proves
// recovery from a state the test invented, and this proves recovery from the
// state the code actually produces.
func (h *harness) strand(t *testing.T, email, displayName string) {
	t.Helper()

	h.store.mu.Lock()
	h.store.failTenantWith = errSecondTransactionLost
	h.store.mu.Unlock()

	_, err := h.service.Register(context.Background(), auth.RegisterInput{
		Email:       email,
		Password:    testPassword,
		DisplayName: displayName,
	})
	if err == nil {
		t.Fatal("Register succeeded with a failing tenant door; the partial state was never created")
	}

	h.store.mu.Lock()
	h.store.failTenantWith = nil
	h.store.mu.Unlock()

	// The account has to exist and be unable to log in, or the rest of the test
	// is recovering from nothing.
	if _, lerr := h.service.Login(context.Background(), auth.LoginInput{
		Email: email, Password: testPassword, ClientIP: "198.51.100.7",
	}); !errors.Is(lerr, auth.ErrNoOrganization) {
		t.Fatalf("Login for a half-registered account = %v, want %v", lerr, auth.ErrNoOrganization)
	}
}

func TestAnAccountStrandedByAFailedRegistrationCanRecoverItself(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	const email = "stranded@example.com"

	h.strand(t, email, "Stranded Person")

	// Acceptance criterion 4: the operator's only signal that this happened.
	if logs := h.logs.String(); !strings.Contains(logs, "auth.register.partial") {
		t.Error("the failed registration did not log auth.register.partial; " +
			"an operator would have no signal that an account was left without an organization")
	}

	// Acceptance criterion 1: no operator involved below this line.
	created, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
		Email:            email,
		Password:         testPassword,
		OrganizationName: "Second Time Lucky",
		ClientIP:         "198.51.100.7",
	})
	if err != nil {
		t.Fatalf("CreateFirstOrganization: %v", err)
	}

	t.Logf("recovered user %s into organization %s (%s)",
		created.UserID, created.OrganizationID, created.OrganizationSlug)

	switch {
	case created.OrganizationID == uuid.Nil:
		t.Fatal("recovery produced no organization")
	case created.OrganizationName != "Second Time Lucky":
		t.Errorf("organization name = %q, want %q", created.OrganizationName, "Second Time Lucky")
	case created.Role != auth.RoleOwner:
		// The whole reason both paths share provisionOrganization: an
		// organization whose creator is not its owner is a permissions bug.
		t.Errorf("role = %q, want %q", created.Role, auth.RoleOwner)
	}

	// And the state is genuinely working, which is what the criterion says —
	// not merely that a row exists.
	result, err := h.service.Login(context.Background(), auth.LoginInput{
		Email: email, Password: testPassword, ClientIP: "198.51.100.7",
	})
	if err != nil {
		t.Fatalf("Login after recovery: %v", err)
	}

	if result.Principal.TenantID != created.OrganizationID {
		t.Errorf("login named tenant %s, want the recovered organization %s",
			result.Principal.TenantID, created.OrganizationID)
	}

	if result.Principal.Role != auth.RoleOwner {
		t.Errorf("login role = %q, want %q", result.Principal.Role, auth.RoleOwner)
	}
}

// TestTheOrganizationIsCreatedForTheVerifiedSubjectAndNoOneElse is the
// authorization claim at the service layer.
//
// There is no user id in [auth.CreateOrganizationInput] to attack with, which is
// most of the answer — so what this asserts is the other half: the subject the
// organization lands on is the one the *password* verified against, and a second
// stranded account sitting next to it is untouched.
func TestTheOrganizationIsCreatedForTheVerifiedSubjectAndNoOneElse(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	h.strand(t, "mine@example.com", "Mine")
	h.strand(t, "theirs@example.com", "Theirs")

	mine := h.userID(t, "mine@example.com")
	theirs := h.userID(t, "theirs@example.com")

	created, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
		Email: "mine@example.com", Password: testPassword, ClientIP: "198.51.100.7",
	})
	if err != nil {
		t.Fatalf("CreateFirstOrganization: %v", err)
	}

	if created.UserID != mine {
		t.Fatalf("organization created for user %s, want the verified subject %s", created.UserID, mine)
	}

	mineOrgs := h.memberships(t, mine)
	theirsOrgs := h.memberships(t, theirs)

	t.Logf("mine=%s has %d membership(s); theirs=%s has %d", mine, len(mineOrgs), theirs, len(theirsOrgs))

	if len(mineOrgs) != 1 || mineOrgs[0].OrganizationID != created.OrganizationID {
		t.Errorf("the verified subject holds %v, want exactly the created organization %s",
			mineOrgs, created.OrganizationID)
	}

	if len(theirsOrgs) != 0 {
		t.Errorf("the other stranded account gained %d membership(s); it must be untouched", len(theirsOrgs))
	}
}

func TestCreatingAFirstOrganizationRefusesAnAccountThatAlreadyHasOne(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	registered := h.register(t, "founder@example.com")

	_, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
		Email: "founder@example.com", Password: testPassword, ClientIP: "198.51.100.7",
	})
	if !errors.Is(err, auth.ErrAlreadyHasOrganization) {
		t.Fatalf("CreateFirstOrganization for an account with an organization = %v, want %v",
			err, auth.ErrAlreadyHasOrganization)
	}

	// Refused *before* anything was written, not after.
	if orgs := h.memberships(t, registered.UserID); len(orgs) != 1 {
		t.Errorf("the account now holds %d memberships, want the 1 it registered with", len(orgs))
	}
}

// TestTheAlreadyHasOneRefusalIsUnreachableWithoutThePassword pins the ordering
// that makes the 409 safe to return at all.
//
// ErrAlreadyHasOrganization tells the caller that an address is registered *and*
// has a workspace. That is only acceptable because it sits behind a correct
// password, and nothing but statement order in CreateFirstOrganization makes it
// so. The tempting optimization — read the memberships first and skip the
// argon2id derivation when the account already has one — would turn this into
// both an existence oracle and a free timing oracle, and would look like a
// performance win to someone who did not know why the order was that way.
//
// So: a registered account with an organization, presented with the *wrong*
// password, must be indistinguishable from any other credential failure.
func TestTheAlreadyHasOneRefusalIsUnreachableWithoutThePassword(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())
	h.register(t, "founder@example.com")

	h.deriver.reset()

	_, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
		Email: "founder@example.com", Password: otherTestPassword, ClientIP: "198.51.100.7",
	})

	t.Logf("an account that has an organization, wrong password -> %v (%d derivations)",
		err, h.deriver.count())

	if errors.Is(err, auth.ErrAlreadyHasOrganization) {
		t.Fatal("a wrong password was answered with ErrAlreadyHasOrganization; the refusal that " +
			"discloses an account has a workspace is reachable without the credential")
	}

	if !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("CreateFirstOrganization = %v, want %v", err, auth.ErrInvalidCredentials)
	}

	// And it cost the same derivation every other credential failure costs, so
	// the membership read was not skipped ahead of the expensive step.
	if got := h.deriver.count(); got != 1 {
		t.Errorf("%d argon2id derivations, want exactly 1", got)
	}
}

// TestCreatingAFirstOrganizationDoesTheSameWorkWhateverIsWrong is
// TestLoginDoesTheSameWorkWhateverIsWrong for this endpoint.
//
// It exists because this is the fourth endpoint that accepts a password from an
// anonymous caller, and an enumeration oracle here would be worth exactly as
// much to an attacker as one on login. The property holds because both call the
// same verifyCredential; this asserts that rather than assuming it, so that
// someone who later gives this endpoint its own credential check finds out.
func TestCreatingAFirstOrganizationDoesTheSameWorkWhateverIsWrong(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())
	h.strand(t, "stranded@example.com", "Stranded Person")

	type observation struct {
		err         error
		derivations int
		reasons     string
	}

	observe := func(t *testing.T, email, password string) observation {
		t.Helper()

		h.deriver.reset()

		h.store.mu.Lock()
		h.store.reasons = nil
		h.store.mu.Unlock()

		_, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
			Email: email, Password: password, ClientIP: "198.51.100.7",
		})

		return observation{err: err, derivations: h.deriver.count(), reasons: h.store.reasonsUsed()}
	}

	cases := map[string]observation{
		"unknown address": observe(t, "nobody-"+uuid.NewString()+"@example.com", testPassword),
		"wrong password":  observe(t, "stranded@example.com", otherTestPassword),
	}

	var reference observation

	for name, got := range cases {
		t.Logf("%-16s -> err=%v derivations=%d pre-tenant reasons=[%s]",
			name, got.err, got.derivations, got.reasons)

		if !errors.Is(got.err, auth.ErrInvalidCredentials) {
			t.Errorf("%s: CreateFirstOrganization = %v, want %v", name, got.err, auth.ErrInvalidCredentials)
		}

		if got.derivations != 1 {
			t.Errorf("%s: %d argon2id derivations, want exactly 1 — a skipped derivation is a timing oracle",
				name, got.derivations)
		}

		if reference.reasons == "" {
			reference = got

			continue
		}

		if got.reasons != reference.reasons {
			t.Errorf("%s used pre-tenant reasons [%s]; the other failing case used [%s]. "+
				"The database does a different shape of work, which is observable",
				name, got.reasons, reference.reasons)
		}
	}
}

// TestCreatingAFirstOrganizationIsRateLimited matters more here than it looks.
//
// Registration is not rate limited despite its doc comment saying so — that is
// issue #73 — so "the unauthenticated endpoints are budgeted" cannot be assumed
// from the neighbours. This endpoint counts its attempts against the same two
// budgets login uses, and an unbudgeted password endpoint is a free offline
// guessing surface, so the assertion is direct.
func TestCreatingAFirstOrganizationIsRateLimited(t *testing.T) {
	t.Parallel()

	h := newHarness(t, auth.RateLimitConfig{PerAccount: 2, PerAddress: 100, Window: time.Minute})
	h.strand(t, "stranded@example.com", "Stranded Person")

	var limited *auth.RateLimitError

	for attempt := 1; attempt <= 3; attempt++ {
		_, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
			Email: "stranded@example.com", Password: otherTestPassword, ClientIP: "198.51.100.7",
		})

		t.Logf("attempt %d: %v", attempt, err)

		if errors.As(err, &limited) {
			return
		}
	}

	t.Error("three wrong-password attempts against a budget of two were never rate limited; " +
		"this endpoint is a free password-guessing surface")
}

func TestARecoveredWorkspaceIsNamedAfterTheAccountWhenNoNameIsGiven(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())
	h.strand(t, "stranded@example.com", "Ada Lovelace")

	created, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
		Email: "stranded@example.com", Password: testPassword, ClientIP: "198.51.100.7",
	})
	if err != nil {
		t.Fatalf("CreateFirstOrganization: %v", err)
	}

	// The same default registration would have produced, from the display name
	// on the account rather than one supplied with the request.
	if want := "Ada Lovelace's workspace"; created.OrganizationName != want {
		t.Errorf("organization name = %q, want %q", created.OrganizationName, want)
	}
}

// userID reads the id the fake assigned to an address.
func (h *harness) userID(t *testing.T, email string) uuid.UUID {
	t.Helper()

	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	user, ok := h.store.usersByEmail[auth.NormalizeEmail(email)]
	if !ok {
		t.Fatalf("no account for %s", email)
	}

	return user.ID
}

// memberships reads the organizations a user belongs to, straight out of the
// fake rather than through the service, so an assertion about who holds what
// cannot be satisfied by the same code path it is checking.
func (h *harness) memberships(t *testing.T, userID uuid.UUID) []store.UserOrganization {
	t.Helper()

	h.store.mu.Lock()
	defer h.store.mu.Unlock()

	return append([]store.UserOrganization(nil), h.store.memberships[userID]...)
}
