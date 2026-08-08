package auth_test

// The service's flows, against fakes.
//
// Two tests here carry most of the weight:
//
//   - TestLoginDoesTheSameWorkWhateverIsWrong, which is the anti-enumeration
//     claim stated as "the same number of derivations, the same pre-tenant
//     calls, the same error" rather than as a wall-clock measurement;
//   - TestSwitchOrganizationRefusesAnOrganizationTheSubjectIsNotIn, which is
//     the BOLA claim at the service layer. The HTTP-layer version lives in
//     internal/api/auth_bola_test.go and is the one that also attacks headers
//     and query parameters.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

const (
	testPassword      = "correct horse battery staple"
	otherTestPassword = "incorrect horse battery staple"
)

type harness struct {
	service  *auth.Service
	store    *fakeStore
	deriver  *countingDeriver
	kv       *memoryKV
	sessions *auth.SessionStore
	issuer   *auth.Issuer
	logs     *strings.Builder
}

func newHarness(t *testing.T, limits auth.RateLimitConfig) *harness {
	t.Helper()

	var logs strings.Builder

	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	kv := newMemoryKV()
	deriver := newCountingDeriver()
	fake := newFakeStore()
	issuer := newTestIssuer(t, testAccessTTL)
	sessions := auth.NewSessionStore(kv, testRefreshTTL)

	service, err := auth.NewService(auth.ServiceDeps{
		Store:      fake,
		Deriver:    deriver,
		Issuer:     issuer,
		Sessions:   sessions,
		Limiter:    auth.NewLimiter(kv, limits, []byte("pepper-for-tests-only-not-a-secret"), logger),
		Logger:     logger,
		Params:     auth.DefaultArgon2Params(),
		AbsentSalt: []byte("sixteen-byte-sal"),
	})
	if err != nil {
		t.Fatalf("building the service: %v", err)
	}

	return &harness{
		service:  service,
		store:    fake,
		deriver:  deriver,
		kv:       kv,
		sessions: sessions,
		issuer:   issuer,
		logs:     &logs,
	}
}

// generousLimits is a budget high enough that the rate limiter never fires,
// for the tests that are about something else.
func generousLimits() auth.RateLimitConfig {
	return auth.RateLimitConfig{PerAccount: 1000, PerAddress: 1000, Window: time.Minute}
}

func (h *harness) register(t *testing.T, email string) auth.RegisterResult {
	t.Helper()

	result, err := h.service.Register(context.Background(), auth.RegisterInput{
		Email:       email,
		Password:    testPassword,
		DisplayName: "Test Person",
	})
	if err != nil {
		t.Fatalf("Register(%s): %v", email, err)
	}

	return result
}

func TestRegisterThenLoginThenTheTokenNamesTheNewOrganization(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	registered := h.register(t, "founder@example.com")

	t.Logf("registered user %s into organization %s (%s)",
		registered.UserID, registered.OrganizationID, registered.OrganizationSlug)

	if registered.OrganizationID == uuid.Nil {
		t.Fatal("registration produced no organization; login would have no tenant to name")
	}

	result, err := h.service.Login(context.Background(), auth.LoginInput{
		Email:    "FOUNDER@example.com", // capitalised: normalisation has to make this the same account
		Password: testPassword,
		ClientIP: "203.0.113.10",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if result.Principal.UserID != registered.UserID {
		t.Errorf("login resolved to user %s, want %s", result.Principal.UserID, registered.UserID)
	}

	if result.Principal.TenantID != registered.OrganizationID {
		t.Errorf("login tenant = %s, want %s", result.Principal.TenantID, registered.OrganizationID)
	}

	if result.Principal.Role != auth.RoleOwner {
		t.Errorf("login role = %q, want %q", result.Principal.Role, auth.RoleOwner)
	}

	// The claim has to survive a round trip through the token, because that is
	// what the middleware will read.
	verified, err := h.issuer.Verify(result.Tokens.AccessToken)
	if err != nil {
		t.Fatalf("the issued access token does not verify: %v", err)
	}

	t.Logf("token claims: sub=%s org=%s role=%s", verified.UserID, verified.TenantID, verified.Role)

	if verified.TenantID != registered.OrganizationID {
		t.Errorf("token org claim = %s, want %s", verified.TenantID, registered.OrganizationID)
	}
}

func TestRegisterRejectsADuplicateAddressAndWeakInput(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())
	h.register(t, "taken@example.com")

	for _, tc := range []struct {
		name string
		in   auth.RegisterInput
		want error
	}{
		{
			name: "duplicate address",
			in:   auth.RegisterInput{Email: "taken@example.com", Password: testPassword, DisplayName: "Someone"},
			want: auth.ErrEmailTaken,
		},
		{
			name: "duplicate address, different capitalisation",
			in:   auth.RegisterInput{Email: "TAKEN@Example.com", Password: testPassword, DisplayName: "Someone"},
			want: auth.ErrEmailTaken,
		},
		{
			name: "password below the floor",
			in:   auth.RegisterInput{Email: "short@example.com", Password: "short", DisplayName: "Someone"},
			want: auth.ErrInvalidInput,
		},
		{
			name: "password above the ceiling",
			in:   auth.RegisterInput{Email: "long@example.com", Password: strings.Repeat("a", 200), DisplayName: "Someone"},
			want: auth.ErrInvalidInput,
		},
		{
			name: "not an address",
			in:   auth.RegisterInput{Email: "nope", Password: testPassword, DisplayName: "Someone"},
			want: auth.ErrInvalidInput,
		},
		{
			name: "no display name",
			in:   auth.RegisterInput{Email: "anon@example.com", Password: testPassword, DisplayName: "   "},
			want: auth.ErrInvalidInput,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.service.Register(context.Background(), tc.in)

			t.Logf("%s -> %v", tc.name, err)

			if !errors.Is(err, tc.want) {
				t.Errorf("Register = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestLoginDoesTheSameWorkWhateverIsWrong is the anti-enumeration test.
//
// It asserts three things about the three failing cases — unknown address,
// wrong password, and an account that exists but has no password:
//
//  1. the same error comes back, so the HTTP layer cannot render them
//     differently even by accident;
//  2. exactly one argon2id derivation happens in each, so the expensive step is
//     not skipped for the absent account — that skip is the classic oracle,
//     and it is worth tens of milliseconds, which is trivially measurable over
//     a network;
//  3. the same sequence of pre-tenant reasons is used, so the *database* does
//     the same shape of work too.
//
// Counting rather than timing: a wall-clock assertion flakes on a shared runner
// and would have to be loose enough to miss a real regression. The integration
// suite carries a loose timing check as well, as a backstop against some other
// step becoming asymmetric.
func TestLoginDoesTheSameWorkWhateverIsWrong(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())
	h.register(t, "real@example.com")

	// An account with no credential — an invited user who never accepted, or
	// later an external-provider-only account.
	passwordless := store.IdentityUser{
		ID:          uuid.New(),
		Email:       "passwordless@example.com",
		DisplayName: "Invited, never accepted",
	}

	h.store.mu.Lock()
	h.store.usersByEmail[passwordless.Email] = passwordless
	h.store.usersByID[passwordless.ID] = passwordless
	h.store.mu.Unlock()

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

		_, err := h.service.Login(context.Background(), auth.LoginInput{
			Email:    email,
			Password: password,
			ClientIP: "198.51.100.7",
		})

		return observation{err: err, derivations: h.deriver.count(), reasons: h.store.reasonsUsed()}
	}

	cases := map[string]observation{
		"unknown address":          observe(t, "nobody-"+uuid.NewString()+"@example.com", testPassword),
		"wrong password":           observe(t, "real@example.com", otherTestPassword),
		"account with no password": observe(t, passwordless.Email, testPassword),
	}

	var reference observation

	for name, got := range cases {
		t.Logf("%-26s -> err=%v derivations=%d pre-tenant reasons=[%s]",
			name, got.err, got.derivations, got.reasons)

		if !errors.Is(got.err, auth.ErrInvalidCredentials) {
			t.Errorf("%s: Login = %v, want %v", name, got.err, auth.ErrInvalidCredentials)
		}

		if got.derivations != 1 {
			t.Errorf("%s: %d argon2id derivations, want exactly 1 — a skipped derivation is a ~%s timing oracle",
				name, got.derivations, "80ms")
		}

		if reference.reasons == "" {
			reference = got

			continue
		}

		if got.reasons != reference.reasons {
			t.Errorf("%s used pre-tenant reasons [%s]; another failing case used [%s]. The database does a different shape of work, which is observable",
				name, got.reasons, reference.reasons)
		}
	}
}

// TestASuccessfulLoginCostsOneDerivationToo keeps the test above honest: if
// success also cost one derivation and failure cost none, the comparison
// between the failures would still pass while the interesting difference —
// success versus failure — leaked.
func TestASuccessfulLoginCostsOneDerivationToo(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())
	h.register(t, "success@example.com")
	h.deriver.reset()

	h.store.mu.Lock()
	h.store.reasons = nil
	h.store.mu.Unlock()

	if _, err := h.service.Login(context.Background(), auth.LoginInput{
		Email: "success@example.com", Password: testPassword, ClientIP: "198.51.100.8",
	}); err != nil {
		t.Fatalf("Login: %v", err)
	}

	t.Logf("successful login: derivations=%d reasons=[%s]", h.deriver.count(), h.store.reasonsUsed())

	if h.deriver.count() != 1 {
		t.Errorf("a successful login cost %d derivations, want 1", h.deriver.count())
	}
}

// TestLoginIsRateLimitedBeforeAnyCredentialWorkHappens checks both that the
// budget applies and that it applies *early* — a limiter that ran after the
// derivation would still let an attacker spend the server's CPU.
func TestLoginIsRateLimitedBeforeAnyCredentialWorkHappens(t *testing.T) {
	t.Parallel()

	h := newHarness(t, auth.RateLimitConfig{PerAccount: 3, PerAddress: 100, Window: time.Minute})
	h.register(t, "target@example.com")
	h.deriver.reset()

	var limited error

	attempts := 0

	for range 6 {
		attempts++

		_, err := h.service.Login(context.Background(), auth.LoginInput{
			Email: "target@example.com", Password: otherTestPassword, ClientIP: "192.0.2.99",
		})
		if errors.Is(err, auth.ErrRateLimited) {
			limited = err

			break
		}
	}

	t.Logf("limited after %d attempts: %v; derivations performed: %d", attempts, limited, h.deriver.count())

	if limited == nil {
		t.Fatal("six attempts against a budget of three were all allowed")
	}

	var rateErr *auth.RateLimitError
	if !errors.As(limited, &rateErr) {
		t.Fatalf("the limit error does not carry a retry duration: %T", limited)
	}

	if rateErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %s, want positive", rateErr.RetryAfter)
	}

	// Three allowed attempts, three derivations. The refused one did no
	// credential work at all.
	if h.deriver.count() != 3 {
		t.Errorf("%d derivations for 3 allowed attempts; the limiter is not running first", h.deriver.count())
	}
}

// TestTheRateLimiterFailsOpen documents a deliberate choice rather than
// discovering one. See Limiter.Allow for why this is defensible here and would
// not be if anything else could issue a session.
func TestTheRateLimiterFailsOpen(t *testing.T) {
	t.Parallel()

	h := newHarness(t, auth.RateLimitConfig{PerAccount: 1, PerAddress: 1, Window: time.Minute})
	h.register(t, "open@example.com")

	h.kv.mu.Lock()
	h.kv.failWith = errors.New("redis is down")
	h.kv.mu.Unlock()

	_, err := h.service.Login(context.Background(), auth.LoginInput{
		Email: "open@example.com", Password: otherTestPassword, ClientIP: "192.0.2.100",
	})

	t.Logf("login with the limiter's store unreachable -> %v", err)

	if errors.Is(err, auth.ErrRateLimited) {
		t.Error("the limiter failed closed; a Redis outage would report as 'too many attempts'")
	}

	if !strings.Contains(h.logs.String(), "login rate limiter unavailable") {
		t.Error("failing open was not logged; it has to be visible or it is indistinguishable from working")
	}
}

// TestSwitchOrganizationRefusesAnOrganizationTheSubjectIsNotIn is the BOLA
// claim at the service layer.
//
// The subject is authenticated and asks for a tenant it has no membership in.
// Two things must be true: no token comes back, and the requested tenant is
// never opened as a tenant context — because store.WithTenant would have served
// it faithfully if it had been.
func TestSwitchOrganizationRefusesAnOrganizationTheSubjectIsNotIn(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice@example.com")
	bob := h.register(t, "bob@example.com")

	t.Logf("alice owns %s; bob owns %s", alice.OrganizationID, bob.OrganizationID)

	principal := auth.Principal{
		UserID:    alice.UserID,
		TenantID:  alice.OrganizationID,
		Role:      auth.RoleOwner,
		SessionID: uuid.New(),
	}

	before := len(h.store.tenantsOpened())

	result, err := h.service.SwitchOrganization(context.Background(), principal, bob.OrganizationID)

	t.Logf("alice asking for bob's organization -> %v", err)

	if !errors.Is(err, auth.ErrNotAMember) {
		t.Fatalf("SwitchOrganization = %v, want %v", err, auth.ErrNotAMember)
	}

	if result.Tokens.AccessToken != "" {
		t.Fatal("a token was issued for an organization the subject does not belong to")
	}

	// The stronger half. Even if the refusal above regressed into returning a
	// token, this would catch a tenant context ever having been opened for
	// bob's organization on alice's behalf.
	for _, opened := range h.store.tenantsOpened()[before:] {
		t.Logf("tenant context opened during the attempt: %s", opened)

		if opened == bob.OrganizationID {
			t.Errorf("a tenant context was opened for %s while acting as a non-member", bob.OrganizationID)
		}
	}
}

// TestSwitchOrganizationWorksForAMembershipTheSubjectHas is the control. Without
// it the test above would also pass if SwitchOrganization refused everything.
func TestSwitchOrganizationWorksForAMembershipTheSubjectHas(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice2@example.com")
	second := uuid.New()

	// A second membership, created the way an invite would: a tenant-scoped
	// insert in the other organization.
	if err := h.store.WithTenant(context.Background(), second, func(ctx context.Context, q store.Querier) error {
		_, cerr := q.CreateMembership(ctx, store.CreateMembershipParams{UserID: alice.UserID, Role: "member"})

		return cerr
	}); err != nil {
		t.Fatalf("seeding a second membership: %v", err)
	}

	principal := auth.Principal{
		UserID:    alice.UserID,
		TenantID:  alice.OrganizationID,
		Role:      auth.RoleOwner,
		SessionID: uuid.New(),
	}

	result, err := h.service.SwitchOrganization(context.Background(), principal, second)
	if err != nil {
		t.Fatalf("SwitchOrganization to a real membership: %v", err)
	}

	t.Logf("switched into %s as %q", result.Principal.TenantID, result.Principal.Role)

	if result.Principal.TenantID != second {
		t.Errorf("switched to %s, want %s", result.Principal.TenantID, second)
	}

	verified, err := h.issuer.Verify(result.Tokens.AccessToken)
	if err != nil {
		t.Fatalf("the new access token does not verify: %v", err)
	}

	if verified.TenantID != second {
		t.Errorf("the new token names %s, want %s", verified.TenantID, second)
	}
}

func TestRefreshRotatesAndRevocationSticks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, generousLimits())
	h.register(t, "refresher@example.com")

	login, err := h.service.Login(ctx, auth.LoginInput{
		Email: "refresher@example.com", Password: testPassword, ClientIP: "192.0.2.5",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	refreshed, err := h.service.Refresh(ctx, login.Tokens.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if refreshed.Tokens.RefreshToken == login.Tokens.RefreshToken {
		t.Error("Refresh returned the same refresh token; rotation did not happen")
	}

	if refreshed.Principal.TenantID != login.Principal.TenantID {
		t.Errorf("refresh changed the tenant: %s -> %s", login.Principal.TenantID, refreshed.Principal.TenantID)
	}

	// Logout, then the refresh token must be dead. This is the acceptance
	// criterion "refresh token revocation works".
	if err := h.service.Logout(ctx, refreshed.Tokens.RefreshToken); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	_, err = h.service.Refresh(ctx, refreshed.Tokens.RefreshToken)

	t.Logf("refreshing a revoked token -> %v", err)

	if !errors.Is(err, auth.ErrRefreshUnknown) {
		t.Errorf("Refresh after Logout = %v, want %v", err, auth.ErrRefreshUnknown)
	}
}

// TestRefreshStopsWorkingWhenTheMembershipIsGone is the reason refresh consults
// the database at all. Without it, removing someone from an organization would
// leave them able to keep renewing a token for it until the refresh ttl ran out
// — days rather than the access token's minutes.
func TestRefreshStopsWorkingWhenTheMembershipIsGone(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, generousLimits())
	registered := h.register(t, "removed@example.com")

	login, err := h.service.Login(ctx, auth.LoginInput{
		Email: "removed@example.com", Password: testPassword, ClientIP: "192.0.2.6",
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// The membership disappears, as it would if an admin removed them.
	h.store.mu.Lock()
	delete(h.store.memberships, registered.UserID)
	h.store.mu.Unlock()

	_, err = h.service.Refresh(ctx, login.Tokens.RefreshToken)

	t.Logf("refreshing after the membership was removed -> %v", err)

	if !errors.Is(err, auth.ErrNotAMember) {
		t.Errorf("Refresh = %v, want %v", err, auth.ErrNotAMember)
	}
}

// TestNothingSensitiveReachesTheLogs greps the service's own output.
//
// Requirement 7 of the issue asks for this to be verified rather than assumed,
// and a test is a better place to verify it than a one-off grep of a diff:
// this one keeps holding when someone adds a log line six months from now.
func TestNothingSensitiveReachesTheLogs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t, generousLimits())

	const (
		email    = "logged@example.com"
		password = "a-very-distinctive-password-value"
	)

	if _, err := h.service.Register(ctx, auth.RegisterInput{
		Email: email, Password: password, DisplayName: "Logged Person",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	login, err := h.service.Login(ctx, auth.LoginInput{Email: email, Password: password, ClientIP: "192.0.2.7"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if _, err := h.service.Login(ctx, auth.LoginInput{Email: email, Password: "wrong-" + password, ClientIP: "192.0.2.7"}); err == nil {
		t.Fatal("a wrong password logged in")
	}

	if _, err := h.service.Refresh(ctx, login.Tokens.RefreshToken); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	output := h.logs.String()

	t.Logf("%d bytes of auth log emitted", len(output))

	for _, secret := range []struct {
		name  string
		value string
	}{
		{name: "the password", value: password},
		{name: "the access token", value: login.Tokens.AccessToken},
		{name: "the refresh token", value: login.Tokens.RefreshToken},
		{name: "the email address", value: email},
	} {
		if secret.value == "" {
			t.Fatalf("%s is empty; this test would pass vacuously", secret.name)
		}

		if strings.Contains(output, secret.value) {
			t.Errorf("%s appears in the logs", secret.name)
		}
	}

	// And the control: the log is not empty of the things it *should* carry,
	// or the assertions above would hold for a logger writing nothing.
	for _, want := range []string{"auth.register.success", "auth.login.success", "auth.login.failed", "auth.refresh.success"} {
		if !strings.Contains(output, want) {
			t.Errorf("the logs do not contain %q; auth events are not being recorded", want)
		}
	}
}

func TestNewServiceRefusesAnIncompleteWiring(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	kv := newMemoryKV()

	complete := func(t *testing.T) auth.ServiceDeps {
		t.Helper()

		return auth.ServiceDeps{
			Store:      newFakeStore(),
			Deriver:    auth.NewArgon2Deriver(1),
			Issuer:     newTestIssuer(t, testAccessTTL),
			Sessions:   auth.NewSessionStore(kv, testRefreshTTL),
			Limiter:    auth.NewLimiter(kv, generousLimits(), []byte("pepper"), logger),
			Logger:     logger,
			Params:     auth.DefaultArgon2Params(),
			AbsentSalt: []byte("sixteen-byte-sal"),
		}
	}

	for _, tc := range []struct {
		name  string
		mutta func(*auth.ServiceDeps)
	}{
		{name: "no store", mutta: func(d *auth.ServiceDeps) { d.Store = nil }},
		{name: "no deriver", mutta: func(d *auth.ServiceDeps) { d.Deriver = nil }},
		{name: "no issuer", mutta: func(d *auth.ServiceDeps) { d.Issuer = nil }},
		{name: "no sessions", mutta: func(d *auth.ServiceDeps) { d.Sessions = nil }},
		{name: "no limiter", mutta: func(d *auth.ServiceDeps) { d.Limiter = nil }},
		{name: "no logger", mutta: func(d *auth.ServiceDeps) { d.Logger = nil }},
		{name: "no absent salt", mutta: func(d *auth.ServiceDeps) { d.AbsentSalt = nil }},
		{name: "short absent salt", mutta: func(d *auth.ServiceDeps) { d.AbsentSalt = []byte("short") }},
		{name: "weakened argon2 memory", mutta: func(d *auth.ServiceDeps) { d.Params.MemoryKiB = 1024 }},
		{name: "weakened argon2 iterations", mutta: func(d *auth.ServiceDeps) { d.Params.Iterations = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			deps := complete(t)
			tc.mutta(&deps)

			_, err := auth.NewService(deps)

			t.Logf("%s -> %v", tc.name, err)

			if err == nil {
				t.Error("NewService accepted a wiring it should refuse")
			}
		})
	}
}
