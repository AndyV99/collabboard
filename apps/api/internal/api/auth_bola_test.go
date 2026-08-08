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
}

func newRecordingStore() *recordingStore {
	return &recordingStore{members: map[uuid.UUID][]store.ListMembersRow{}}
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

func (r *recordingStore) seed(tenantID uuid.UUID, email string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.members[tenantID] = []store.ListMembersRow{{
		MembershipID: uuid.New(),
		UserID:       uuid.New(),
		Role:         "owner",
		Email:        email,
		DisplayName:  email,
	}}
}

type recordingQuerier struct {
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

func (q recordingQuerier) CreateOrganization(context.Context, store.CreateOrganizationParams) (store.Organization, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) CreateMembership(context.Context, store.CreateMembershipParams) (store.Membership, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) ListProjects(context.Context) ([]store.Project, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) CreateProject(context.Context, store.CreateProjectParams) (store.Project, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) GetBoard(context.Context, uuid.UUID) (store.Board, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) GetMembership(context.Context, uuid.UUID) (store.Membership, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) ListColumnsByBoard(context.Context, uuid.UUID) ([]store.Column, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) ListCardsByBoard(context.Context, uuid.UUID) ([]store.Card, error) {
	panic("recordingQuerier: not modelled")
}

// membershipService is an AuthService whose only real behaviour is membership:
// it knows which organizations each user belongs to, which is precisely what
// SwitchOrganization has to check.
type membershipService struct {
	issuer      *auth.Issuer
	memberships map[uuid.UUID][]auth.Organization
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

func (s *membershipService) Organizations(_ context.Context, principal auth.Principal) ([]auth.Organization, error) {
	return s.memberships[principal.UserID], nil
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
}

func newBOLAFixture(t *testing.T) *bolaFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	issuer := testIssuer(t)

	tenantA := uuid.New()
	tenantB := uuid.New()

	alice := uuid.New()

	tenantStore := newRecordingStore()
	tenantStore.seed(tenantA, "alice@example.com")
	tenantStore.seed(tenantB, "bob@example.com")

	service := &membershipService{
		issuer: issuer,
		memberships: map[uuid.UUID][]auth.Organization{
			// Alice belongs to A and to nothing else. Bob's organization exists
			// and has data; that is the whole setup.
			alice: {{ID: tenantA, Name: "Alice Co", Slug: "alice-co", Role: "owner"}},
		},
	}

	router := NewRouter(discardLogger(),
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

	for _, attack := range []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		body    any
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
		{
			name:   "a path segment naming the organization",
			method: http.MethodGet,
			path:   "/api/v1/organizations/" + f.tenantB.String() + "/members",
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

			f.assertNoForeignTenantOpened(t, before)
		})
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
