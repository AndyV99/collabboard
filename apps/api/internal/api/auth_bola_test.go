package api

// The BOLA test: an authenticated member of organization A cannot obtain
// organization B's tenant context, whatever they put in the request.
//
// # Why this is the most important file in the change
//
// store.WithTenant sets app.tenant_id to whatever it is handed, and Postgres
// serves that tenant faithfully. ADR 0001 is explicit that the row-level
// security layer is isolation and not authorization. So there is exactly one
// thing standing between a customer and another customer's board: the fact that
// the tenant id comes from a signed claim rather than from the request. If a
// request can name its own tenant and be believed, RLS will cheerfully serve
// the wrong organization and every isolation test in internal/store will still
// pass.
//
// This file attacks that from the outside, through the real router, using every
// channel the HTTP surface makes plausible:
//
//   - request headers, including the ones an API of this shape usually has
//     (X-Tenant-ID, X-Organization-ID);
//   - query parameters;
//   - request body fields;
//   - a path segment;
//   - the org claim in the token itself, rewritten;
//   - and the one endpoint that legitimately takes an organization id from a
//     client, POST /api/v1/auth/organization.
//
// Two assertions per attack, and the second is the stronger one:
//
//  1. the response is either a refusal or organization A's data; and
//  2. no tenant context was ever *opened* for organization B — checked by
//     recording every tenant id passed to WithTenant. A handler that leaked and
//     then filtered would pass (1) and fail (2), and it would be one refactor
//     away from leaking for real.
//
// TestTheAssertionHasTeeth at the bottom builds a deliberately vulnerable
// variant — a middleware that honours X-Organization-ID, which is exactly the
// design this one refuses — and shows the same attack succeeding against it. A
// security test that has never been observed to fail is a security test nobody
// should trust.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// recordingStore stands in for internal/store and records every tenant a
// transaction was opened for.
//
// It is not a fake database. It exists to answer one question — "was a tenant
// context ever opened for organization B" — which is a question about the call,
// not about the rows.
type recordingStore struct {
	mu      sync.Mutex
	opened  []uuid.UUID
	members map[uuid.UUID][]store.ListMembersRow

	// added records every membership POST /api/v1/members created, keyed by the
	// tenant it landed in. "The response did not leak" is a weaker claim for a
	// write than for a read: an addition into the wrong organization is a
	// durable change that a 201 with an innocuous body would hide completely.
	added map[uuid.UUID][]uuid.UUID
}

func newRecordingStore() *recordingStore {
	return &recordingStore{
		members: map[uuid.UUID][]store.ListMembersRow{},
		added:   map[uuid.UUID][]uuid.UUID{},
	}
}

// recordAddition notes that a user was joined to a tenant.
func (r *recordingStore) recordAddition(tenantID, userID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.added[tenantID] = append(r.added[tenantID], userID)
}

func (r *recordingStore) additionsTo(tenantID uuid.UUID) []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.added[tenantID])
}

func (r *recordingStore) WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error {
	r.mu.Lock()
	r.opened = append(r.opened, tenantID)
	r.mu.Unlock()

	return fn(ctx, recordingQuerier{store: r, tenantID: tenantID})
}

func (r *recordingStore) openedTenants() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.opened)
}

func (r *recordingStore) seed(tenantID, userID uuid.UUID, email string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.members[tenantID] = []store.ListMembersRow{{
		MembershipID: uuid.New(),
		UserID:       userID,
		Role:         "owner",
		Email:        email,
		DisplayName:  email,
	}}
}

// recordingQuerier models the one query this fixture needs and nothing else.
//
// The embedded interface is nil: calling any other query panics with a nil
// dereference naming the method. That is the same hard failure the hand-written
// panics gave, without having to grow by one stub every time query.sql grows a
// query — which, since #47, it does often.
type recordingQuerier struct {
	store.Querier

	store    *recordingStore
	tenantID uuid.UUID
}

// ListMembers returns only the tenant's own rows, exactly as the RLS policy
// would. That is what makes the leak visible in the response body as well as in
// the recorded tenant list.
func (q recordingQuerier) ListMembers(context.Context) ([]store.ListMembersRow, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	return q.store.members[q.tenantID], nil
}

// GetUser models users_visible_via_membership rather than the users table: a
// row comes back only when a membership joins it to *this transaction's*
// tenant, which is precisely what the policy does.
//
// Modelling it that way is what gives the /me attacks below their teeth. A fake
// that returned the row for any id would answer an attacker who managed to get
// somebody else's id into the query, and the test would pass while the real
// database was the only thing refusing.
func (q recordingQuerier) GetUser(_ context.Context, userID uuid.UUID) (store.GetUserRow, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	for _, member := range q.store.members[q.tenantID] {
		if member.UserID == userID {
			return store.GetUserRow{ID: member.UserID, Email: member.Email, DisplayName: member.DisplayName}, nil
		}
	}

	return store.GetUserRow{}, store.ErrNoRows
}

// membershipService is an AuthService whose only real behaviour is membership:
// it knows which organizations each user belongs to, which is precisely what
// SwitchOrganization has to check.
type membershipService struct {
	issuer      *auth.Issuer
	memberships map[uuid.UUID][]auth.Organization

	// store and accounts exist for AddMember. accounts is the global directory
	// the real service reaches through the pre-tenant door: an address that is
	// not in it has no account anywhere.
	store    *recordingStore
	accounts map[string]uuid.UUID
}

func (s *membershipService) Register(context.Context, auth.RegisterInput) (auth.RegisterResult, error) {
	panic("membershipService: Register is not modelled")
}

func (s *membershipService) Login(context.Context, auth.LoginInput) (auth.LoginResult, error) {
	panic("membershipService: Login is not modelled")
}

func (s *membershipService) Refresh(context.Context, string) (auth.LoginResult, error) {
	panic("membershipService: Refresh is not modelled")
}

func (s *membershipService) Logout(context.Context, string) error {
	panic("membershipService: Logout is not modelled")
}

// Me models both halves of GET /me the way the real service does them: the
// organization list from the principal's user id, and the identity from a
// tenant-scoped read of the principal's own row.
//
// The tenant-scoped half goes through s.store deliberately, so that an attack
// which managed to steer /me would show up in recordingStore.opened — the same
// second assertion every other attack in this file gets. A fake that assembled
// the profile from a map would make /me the one endpoint here whose steering
// nobody was watching.
func (s *membershipService) Me(ctx context.Context, principal auth.Principal) (auth.MeResult, error) {
	var row store.GetUserRow

	err := s.store.WithTenant(ctx, principal.TenantID, func(ctx context.Context, q store.Querier) error {
		var qerr error

		row, qerr = q.GetUser(ctx, principal.UserID)

		return qerr
	})

	switch {
	case errors.Is(err, store.ErrNoRows):
		return auth.MeResult{}, auth.ErrNotAMember
	case err != nil:
		return auth.MeResult{}, err
	}

	return auth.MeResult{
		Profile: auth.UserProfile{
			UserID:      row.ID,
			Email:       row.Email,
			DisplayName: row.DisplayName,
		},
		Organizations: s.memberships[principal.UserID],
	}, nil
}

// AddMember is modelled at the size these tests need, and the size is chosen to
// make the attack *possible*.
//
// It does not model the role ladder — internal/auth/members_test.go owns that,
// against a fake that models memberships. What it models is the only thing this
// file is about: the tenant an addition lands in comes from
// in.Principal.TenantID, and the transaction is opened for that tenant, so a
// principal that was allowed to name another organization would show up both in
// recordingStore.opened and in recordingStore.added. A fake that ignored the
// principal's tenant would make every assertion below vacuous.
func (s *membershipService) AddMember(ctx context.Context, in auth.AddMemberInput) (auth.AddMemberResult, error) {
	userID, registered := s.accounts[auth.NormalizeEmail(in.Email)]
	if !registered {
		return auth.AddMemberResult{}, auth.ErrNoSuchAccount
	}

	result := auth.AddMemberResult{
		MembershipID: uuid.New(),
		UserID:       userID,
		Email:        auth.NormalizeEmail(in.Email),
		Role:         auth.RoleMember,
	}

	err := s.store.WithTenant(ctx, in.Principal.TenantID, func(_ context.Context, _ store.Querier) error {
		s.store.recordAddition(in.Principal.TenantID, userID)

		return nil
	})
	if err != nil {
		return auth.AddMemberResult{}, err
	}

	return result, nil
}

// SwitchOrganization is the real check, reimplemented here at the size the test
// needs: the target must appear in the *authenticated subject's* memberships.
func (s *membershipService) SwitchOrganization(_ context.Context, principal auth.Principal, target uuid.UUID) (auth.LoginResult, error) {
	for _, org := range s.memberships[principal.UserID] {
		if org.ID != target {
			continue
		}

		newPrincipal := auth.Principal{
			UserID:    principal.UserID,
			TenantID:  org.ID,
			Role:      org.Role,
			SessionID: uuid.New(),
		}

		token, expires, err := s.issuer.Issue(newPrincipal)
		if err != nil {
			return auth.LoginResult{}, err
		}

		newPrincipal.ExpiresAt = expires

		return auth.LoginResult{Principal: newPrincipal, Tokens: auth.TokenPair{AccessToken: token}, Organization: org}, nil
	}

	return auth.LoginResult{}, auth.ErrNotAMember
}

type bolaFixture struct {
	router     *gin.Engine
	store      *recordingStore
	issuer     *auth.Issuer
	aliceToken string
	tenantA    uuid.UUID
	tenantB    uuid.UUID

	// aliceID and bobID are what the /me attacks need: an id to ask for, and an
	// id to check the answer against. Alice knows bob's id in the same way an
	// attacker would — she does not, which is why the attacks also try his
	// address.
	aliceID uuid.UUID
	bobID   uuid.UUID

	// carol has an account and belongs to neither organization. She is what
	// alice tries to add — to her own organization in the control, and to bob's
	// in the attack.
	carolEmail string
	carolID    uuid.UUID
}

func newBOLAFixture(t *testing.T) *bolaFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	issuer := testIssuer(t)

	tenantA := uuid.New()
	tenantB := uuid.New()

	alice := uuid.New()
	bob := uuid.New()

	tenantStore := newRecordingStore()
	tenantStore.seed(tenantA, alice, "alice@example.com")
	tenantStore.seed(tenantB, bob, "bob@example.com")

	carol := uuid.New()

	service := &membershipService{
		issuer: issuer,
		memberships: map[uuid.UUID][]auth.Organization{
			// Alice belongs to A and to nothing else. Bob's organization exists
			// and has data; that is the whole setup.
			alice: {{ID: tenantA, Name: "Alice Co", Slug: "alice-co", Role: "owner"}},
		},
		store: tenantStore,
		accounts: map[string]uuid.UUID{
			"alice@example.com": alice,
			"carol@example.com": carol,
		},
	}

	router := NewRouter(discardLogger(),
		BodyLimits{},
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
		AuthDeps{Service: service, Verifier: issuer, Store: tenantStore}, RealtimeDeps{})

	token, _, err := issuer.Issue(auth.Principal{
		UserID:    alice,
		TenantID:  tenantA,
		Role:      "owner",
		SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing alice's token: %v", err)
	}

	return &bolaFixture{
		router:     router,
		store:      tenantStore,
		issuer:     issuer,
		aliceToken: token,
		tenantA:    tenantA,
		tenantB:    tenantB,
		aliceID:    alice,
		bobID:      bob,
		carolEmail: "carol@example.com",
		carolID:    carol,
	}
}

// TestAnAuthenticatedUserCannotObtainAnotherTenantsContext is the headline.
func TestAnAuthenticatedUserCannotObtainAnotherTenantsContext(t *testing.T) {
	t.Parallel()

	f := newBOLAFixture(t)

	t.Logf("alice is a member of %s only; %s belongs to bob", f.tenantA, f.tenantB)

	// The control: alice's own request works and returns alice's data. Without
	// it, every assertion below would also hold for a router that returned 500
	// to everything.
	t.Run("control: alice reads her own members", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/members", nil, nil)

		t.Logf("control -> %d %s", rec.Code, rec.Body.String())

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the fixture is broken and nothing below proves anything", rec.Code)
		}

		if !bytes.Contains(rec.Body.Bytes(), []byte("alice@example.com")) {
			t.Fatalf("alice cannot see her own organization's members: %s", rec.Body.String())
		}
	})

	// The second control, for the /me attacks below. Without it they would all
	// hold against a /me that returned no identity at all — "the response does
	// not contain bob's address" is trivially true of an empty answer.
	t.Run("control: alice reads her own identity from /me", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, "/api/v1/me", nil, nil)

		t.Logf("control -> %d %s", rec.Code, rec.Body.String())

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — nothing below proves anything", rec.Code)
		}

		var body struct {
			UserID      string `json:"user_id"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		}

		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decoding /me: %v", err)
		}

		if body.UserID != f.aliceID.String() {
			t.Errorf("/me user_id = %q, want alice (%s)", body.UserID, f.aliceID)
		}

		if body.Email != "alice@example.com" || body.DisplayName != "alice@example.com" {
			t.Errorf("/me identity = %q / %q, want alice's own", body.Email, body.DisplayName)
		}
	})

	for _, attack := range []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		body    any

		// identity marks an attack on GET /me's *subject* rather than on its
		// tenant. For those, "the body does not contain bob" is not enough: a
		// response steered to some third account would satisfy it too. So a 200
		// has to be alice's own row, positively.
		identity bool
	}{
		{
			name:    "X-Tenant-ID header",
			method:  http.MethodGet,
			path:    "/api/v1/members",
			headers: map[string]string{"X-Tenant-ID": f.tenantB.String()},
		},
		{
			name:    "X-Organization-ID header",
			method:  http.MethodGet,
			path:    "/api/v1/members",
			headers: map[string]string{"X-Organization-ID": f.tenantB.String()},
		},
		{
			name:    "X-Org-Id header",
			method:  http.MethodGet,
			path:    "/api/v1/members",
			headers: map[string]string{"X-Org-Id": f.tenantB.String()},
		},
		{
			name:   "org query parameter",
			method: http.MethodGet,
			path:   "/api/v1/members?org=" + f.tenantB.String(),
		},
		{
			name:   "tenant_id query parameter",
			method: http.MethodGet,
			path:   "/api/v1/members?tenant_id=" + f.tenantB.String(),
		},
		{
			name:   "organization_id query parameter on /me",
			method: http.MethodGet,
			path:   "/api/v1/me?organization_id=" + f.tenantB.String(),
		},
		// /me now reports an identity as well as an organization (#75), so it
		// has a second thing worth stealing: somebody else's row. These attack
		// the *subject* rather than the tenant. The two assertions the loop
		// makes cover both halves — bob's address must not appear in the body,
		// and no transaction may be opened for bob's tenant.
		{
			name:     "user_id query parameter on /me",
			method:   http.MethodGet,
			path:     "/api/v1/me?user_id=" + f.bobID.String(),
			identity: true,
		},
		{
			name:     "sub query parameter on /me",
			method:   http.MethodGet,
			path:     "/api/v1/me?sub=" + f.bobID.String(),
			identity: true,
		},
		{
			name:     "email query parameter on /me",
			method:   http.MethodGet,
			path:     "/api/v1/me?email=bob%40example.com",
			identity: true,
		},
		{
			name:     "X-User-ID header on /me",
			method:   http.MethodGet,
			path:     "/api/v1/me",
			headers:  map[string]string{"X-User-Id": f.bobID.String()},
			identity: true,
		},
		{
			name:     "X-User-ID and X-Organization-ID together on /me",
			method:   http.MethodGet,
			path:     "/api/v1/me",
			headers:  map[string]string{"X-User-Id": f.bobID.String(), "X-Organization-ID": f.tenantB.String()},
			identity: true,
		},
		{
			name:     "a user id in the path",
			method:   http.MethodGet,
			path:     "/api/v1/users/" + f.bobID.String(),
			identity: true,
		},
		{
			name:   "a path segment naming the organization",
			method: http.MethodGet,
			path:   "/api/v1/organizations/" + f.tenantB.String() + "/members",
		},
		// Adding a member (#61) is the first *write* on this surface, so the
		// same channels are attacked again against it. A read that leaks shows
		// up in the response; a write that leaks does not have to, which is why
		// assertNoMemberAddedTo runs below as well.
		{
			name:    "X-Organization-ID header on an addition",
			method:  http.MethodPost,
			path:    "/api/v1/members",
			headers: map[string]string{"X-Organization-ID": f.tenantB.String()},
			body:    map[string]string{"email": f.carolEmail},
		},
		{
			name:   "an organization_id field in the addition's body",
			method: http.MethodPost,
			path:   "/api/v1/members",
			body: map[string]string{
				"email":           f.carolEmail,
				"organization_id": f.tenantB.String(),
				"tenant_id":       f.tenantB.String(),
			},
		},
		{
			name:   "an organization in the path of an addition",
			method: http.MethodPost,
			path:   "/api/v1/organizations/" + f.tenantB.String() + "/members",
			body:   map[string]string{"email": f.carolEmail},
		},
		{
			name:   "an org query parameter on an addition",
			method: http.MethodPost,
			path:   "/api/v1/members?org=" + f.tenantB.String(),
			body:   map[string]string{"email": f.carolEmail},
		},
	} {
		t.Run(attack.name, func(t *testing.T) {
			before := len(f.store.openedTenants())

			rec := f.do(t, attack.method, attack.path, attack.headers, attack.body)

			t.Logf("%s -> %d %s", attack.name, rec.Code, truncate(rec.Body.String()))

			// Whatever the status, the response must not carry organization
			// B's data.
			if bytes.Contains(rec.Body.Bytes(), []byte("bob@example.com")) {
				t.Errorf("BOLA: the response contains another organization's data\n%s", rec.Body.String())
			}

			if bytes.Contains(rec.Body.Bytes(), []byte(f.tenantB.String())) {
				t.Errorf("BOLA: the response echoes the requested foreign tenant id\n%s", rec.Body.String())
			}

			if attack.identity && rec.Code == http.StatusOK {
				f.assertIdentityIsAlices(t, rec)
			}

			f.assertNoForeignTenantOpened(t, before)
			f.assertNoMemberAddedTo(t, f.tenantB)
		})
	}
}

// TestAddingAMemberLandsInTheCallersOwnOrganization is the control for the four
// addition attacks above.
//
// Without it they would all hold just as well against a router with no POST
// /api/v1/members at all: 404 is not a leak, and neither is a route that does
// not exist.
func TestAddingAMemberLandsInTheCallersOwnOrganization(t *testing.T) {
	t.Parallel()

	f := newBOLAFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/members", nil, map[string]string{"email": f.carolEmail})

	t.Logf("alice adding carol to her own organization -> %d %s", rec.Code, truncate(rec.Body.String()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	added := f.store.additionsTo(f.tenantA)

	t.Logf("memberships created in alice's organization: %v", added)

	if !slices.Contains(added, f.carolID) {
		t.Fatalf("the addition reported success but created no membership in %s", f.tenantA)
	}

	f.assertNoMemberAddedTo(t, f.tenantB)
}

// TestAddingAMemberByEmailIsNotADirectoryLookup.
//
// The refusal for an address with no account must carry nothing but a fixed
// sentence: no user id, no display name, and nothing about any organization.
// The status is 404 and that does say "no account with this address" — see
// internal/auth/members.go for why that bit is inherent to the operation and
// what bounds it.
func TestAddingAMemberByEmailIsNotADirectoryLookup(t *testing.T) {
	t.Parallel()

	f := newBOLAFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/members", nil,
		map[string]string{"email": "definitely-nobody@example.com"})

	t.Logf("adding an address with no account -> %d %s", rec.Code, rec.Body.String())

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}

	if len(body) != 1 || body["error"] == nil {
		t.Errorf("the refusal carries %d field(s), want exactly one (\"error\"): %s", len(body), rec.Body.String())
	}

	// And it must be the same sentence whatever was asked, so that two probes
	// cannot be told apart by their bodies.
	other := f.do(t, http.MethodPost, "/api/v1/members", nil,
		map[string]string{"email": "someone-else-entirely@example.org"})

	t.Logf("a second unregistered address -> %d %s", other.Code, other.Body.String())

	if other.Code != rec.Code || other.Body.String() != rec.Body.String() {
		t.Errorf("two unregistered addresses answered differently (%d %s vs %d %s)",
			rec.Code, rec.Body.String(), other.Code, other.Body.String())
	}
}

// TestRewritingTheOrgClaimDoesNotWork is the same attack against the only
// channel that would actually carry a tenant if it were believed.
func TestRewritingTheOrgClaimDoesNotWork(t *testing.T) {
	t.Parallel()

	f := newBOLAFixture(t)

	before := len(f.store.openedTenants())

	forged := tamperClaim(t, f.aliceToken, "org", f.tenantB.String())

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/members", nil)
	req.Header.Set("Authorization", "Bearer "+forged)
	f.router.ServeHTTP(rec, req)

	t.Logf("token with a rewritten org claim -> %d %s", rec.Code, truncate(rec.Body.String()))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — the signature no longer covers the payload", rec.Code)
	}

	f.assertNoForeignTenantOpened(t, before)
}

// TestSwitchingToAnUnjoinedOrganizationIsRefused attacks the one endpoint that
// legitimately accepts an organization id.
//
// This is where the vulnerability would live in a real service: the endpoint
// exists, the id is client-supplied, and the only thing between it and a tenant
// context is a membership check.
func TestSwitchingToAnUnjoinedOrganizationIsRefused(t *testing.T) {
	t.Parallel()

	f := newBOLAFixture(t)

	before := len(f.store.openedTenants())

	rec := f.do(t, http.MethodPost, "/api/v1/auth/organization", nil,
		map[string]string{"organization_id": f.tenantB.String()})

	t.Logf("alice switching into bob's organization -> %d %s", rec.Code, truncate(rec.Body.String()))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body: %v", err)
	}

	if _, hasToken := body["access_token"]; hasToken {
		t.Fatal("a token was issued for an organization the subject does not belong to")
	}

	f.assertNoForeignTenantOpened(t, before)

	// And the refusal must not depend on the organization not existing: a
	// service that answered 404 for a real-but-unjoined organization and 403
	// for a fictional one would be a membership oracle. Both are 403 here.
	fictional := f.do(t, http.MethodPost, "/api/v1/auth/organization", nil,
		map[string]string{"organization_id": uuid.NewString()})

	t.Logf("switching into an organization that does not exist -> %d", fictional.Code)

	if fictional.Code != rec.Code {
		t.Errorf("an unjoined-but-real organization answers %d and a fictional one answers %d; the difference discloses existence",
			rec.Code, fictional.Code)
	}
}

// TestTheAssertionHasTeeth shows the same attack succeeding against a
// deliberately vulnerable router.
//
// The vulnerable middleware is not a straw man: "read the tenant from
// X-Organization-ID so the client can pick a workspace" is the single most
// common way this API shape is built, and it is a textbook BOLA. This test
// fails — loudly — if the vulnerability ever stops being detectable, which is
// what keeps the tests above from being decoration.
func TestTheAssertionHasTeeth(t *testing.T) {
	t.Parallel()

	f := newBOLAFixture(t)

	gin.SetMode(gin.TestMode)

	vulnerable := gin.New()
	vulnerable.GET("/api/v1/members",
		requireAuth(discardLogger(), f.issuer),
		// The bug, in one middleware: a header is allowed to override the
		// claim. Everything downstream is the real code.
		func(c *gin.Context) {
			if header := c.GetHeader("X-Organization-ID"); header != "" {
				if id, err := uuid.Parse(header); err == nil {
					principal, _ := principalFrom(c)
					principal.TenantID = id
					c.Set(principalKey, principal)
				}
			}

			c.Next()
		},
		membersHandler(discardLogger(), f.store))

	before := len(f.store.openedTenants())

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/members", nil)
	req.Header.Set("Authorization", "Bearer "+f.aliceToken)
	req.Header.Set("X-Organization-ID", f.tenantB.String())
	vulnerable.ServeHTTP(rec, req)

	t.Logf("the same request against a middleware that trusts the header -> %d %s",
		rec.Code, truncate(rec.Body.String()))

	// The vulnerable router must leak. If it does not, the detection above is
	// measuring nothing.
	if !bytes.Contains(rec.Body.Bytes(), []byte("bob@example.com")) {
		t.Fatal("the deliberately vulnerable router did not leak; the BOLA assertions above cannot be trusted to detect one")
	}

	opened := f.store.openedTenants()[before:]

	t.Logf("tenant contexts opened by the vulnerable router: %v", opened)

	if !slices.Contains(opened, f.tenantB) {
		t.Fatal("the vulnerable router did not open a foreign tenant context; assertNoForeignTenantOpened cannot detect one either")
	}

	t.Log("confirmed: bypassing the tenant-from-claim rule leaks another organization's members, and both assertions catch it")
}

// assertNoMemberAddedTo checks that no membership was created in an
// organization the caller does not belong to.
//
// Separate from assertNoForeignTenantOpened because they fail separately, and
// because this is the one that survives a refactor: a handler could stop
// opening a foreign tenant context and still write into one through a
// misdirected query. For a write, "nothing happened over there" is the claim,
// not "nothing came back from over there".
func (f *bolaFixture) assertNoMemberAddedTo(t *testing.T, tenantID uuid.UUID) {
	t.Helper()

	if added := f.store.additionsTo(tenantID); len(added) != 0 {
		t.Errorf("BOLA: %d membership(s) were created in %s while authenticated as a member of %s only: %v",
			len(added), tenantID, f.tenantA, added)
	}
}

// TestTheMembershipAssertionHasTeeth is the companion to
// TestTheAssertionHasTeeth for the write half.
//
// A response-body assertion cannot detect a leaked *write*: the vulnerable
// router below answers 201 with a body that names no organization at all, which
// is exactly what the correct router answers. So the detection has to be the
// store, and this proves the store detects it.
func TestTheMembershipAssertionHasTeeth(t *testing.T) {
	t.Parallel()

	f := newBOLAFixture(t)

	gin.SetMode(gin.TestMode)

	service := &membershipService{
		issuer:   f.issuer,
		store:    f.store,
		accounts: map[string]uuid.UUID{f.carolEmail: f.carolID},
	}

	vulnerable := gin.New()
	vulnerable.POST("/api/v1/members",
		requireAuth(discardLogger(), f.issuer),
		// The bug, in one middleware: a header is allowed to override the
		// claim. Everything downstream is the real handler.
		func(c *gin.Context) {
			if header := c.GetHeader("X-Organization-ID"); header != "" {
				if id, err := uuid.Parse(header); err == nil {
					principal, _ := principalFrom(c)
					principal.TenantID = id
					c.Set(principalKey, principal)
				}
			}

			c.Next()
		},
		addMemberHandler(discardLogger(), service))

	before := len(f.store.openedTenants())

	body, err := json.Marshal(map[string]string{"email": f.carolEmail})
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/members", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.aliceToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Organization-ID", f.tenantB.String())
	vulnerable.ServeHTTP(rec, req)

	t.Logf("the same addition against a middleware that trusts the header -> %d %s",
		rec.Code, truncate(rec.Body.String()))

	if rec.Code != http.StatusCreated {
		t.Fatalf("the deliberately vulnerable router refused the addition (%d); it cannot demonstrate the leak", rec.Code)
	}

	// The response is innocuous — which is the point.
	if bytes.Contains(rec.Body.Bytes(), []byte(f.tenantB.String())) {
		t.Log("note: the vulnerable response echoed the foreign tenant; the store assertion is not the only detector here")
	}

	added := f.store.additionsTo(f.tenantB)

	t.Logf("memberships the vulnerable router created in bob's organization: %v", added)

	if !slices.Contains(added, f.carolID) {
		t.Fatal("the vulnerable router created no membership in the foreign organization; assertNoMemberAddedTo cannot detect one either")
	}

	opened := f.store.openedTenants()[before:]

	if !slices.Contains(opened, f.tenantB) {
		t.Fatal("the vulnerable router opened no foreign tenant context; assertNoForeignTenantOpened cannot detect one either")
	}

	t.Log("confirmed: bypassing the tenant-from-claim rule puts a member into another organization behind a 201 that says nothing, and the store assertion catches it")
}

// assertNoForeignTenantOpened checks every tenant context opened since index
// `from` was alice's own.
func (f *bolaFixture) assertNoForeignTenantOpened(t *testing.T, from int) {
	t.Helper()

	opened := f.store.openedTenants()

	if from > len(opened) {
		from = len(opened)
	}

	for _, tenantID := range opened[from:] {
		t.Logf("tenant context opened: %s", tenantID)

		if tenantID != f.tenantA {
			t.Errorf("BOLA: a tenant context was opened for %s while authenticated as a member of %s only",
				tenantID, f.tenantA)
		}
	}
}

// assertIdentityIsAlices checks that a successful /me answered with the
// caller's own row.
//
// Positive rather than negative on purpose. "The body does not contain bob"
// passes for a response steered to any third account, and for an empty one; the
// claim GET /me makes is that the identity it returns is the *token's*, so that
// is what gets asserted.
func (f *bolaFixture) assertIdentityIsAlices(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	var body struct {
		UserID string `json:"user_id"`
		Email  string `json:"email"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding /me: %v (%s)", err, rec.Body.String())
	}

	if body.UserID != f.aliceID.String() {
		t.Errorf("BOLA: /me reported user_id %q, want alice (%s)", body.UserID, f.aliceID)
	}

	if body.Email != "alice@example.com" {
		t.Errorf("BOLA: /me reported email %q, want alice's own", body.Email)
	}
}

func (f *bolaFixture) do(t *testing.T, method, path string, headers map[string]string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var payload *bytes.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the body: %v", err)
		}

		payload = bytes.NewReader(encoded)
	} else {
		payload = bytes.NewReader(nil)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, payload)
	req.Header.Set("Authorization", "Bearer "+f.aliceToken)
	req.Header.Set("Content-Type", "application/json")

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	return rec
}

// tamperClaim rewrites one claim and leaves the signature untouched.
func tamperClaim(t *testing.T, token, claim, value string) string {
	t.Helper()

	parts := bytes.Split([]byte(token), []byte("."))
	if len(parts) != 3 {
		t.Fatalf("token is not a jwt: %q", token)
	}

	decoded := decodeSegment(t, string(parts[1]))

	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}

	payload[claim] = value

	edited, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}

	return string(parts[0]) + "." + encodeSegment(edited) + "." + string(parts[2])
}

func decodeSegment(t *testing.T, segment string) []byte {
	t.Helper()

	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decoding a token segment: %v", err)
	}

	return decoded
}

func encodeSegment(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

func truncate(s string) string {
	const limit = 240

	if len(s) <= limit {
		return s
	}

	return s[:limit] + "…"
}
