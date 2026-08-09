//go:build integration

package api_test

// The auth surface end to end, against a real Postgres and a real Redis.
//
// Everything here goes over HTTP to an httptest server running the router
// cmd/api builds, backed by the serving role (collabboard_app: not a superuser,
// no BYPASSRLS, owns nothing) and by a Redis container. The unit tests prove the
// logic; these prove the wiring, and — for the two tenant-isolation tests —
// prove it against the database whose policies are the thing being relied on.
//
// The BOLA test in auth_bola_test.go runs against a recording fake, which is
// the right tool for "was a tenant context ever opened". This file runs the
// same attack against a real database, where the answer is not "no context was
// opened" but "the rows that came back belong to the caller".

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/redistest"
)

var (
	testDB    *pgtest.DB
	testRedis *redistest.Server

	// redisDB hands each server its own logical database. The auth key names
	// are hashes and session ids rather than tenant-scoped rows, so nothing but
	// the database index keeps one test's rate-limit counters out of another's.
	redisDB atomic.Int32
)

func TestMain(m *testing.M) {
	// The exit code goes through a function so the deferred teardown runs:
	// os.Exit skips defers.
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	db, err := pgtest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration harness (postgres): %v\n", err)

		// 1, not a skip: a harness that quietly degrades to "no tests ran" when
		// Docker is missing is how a suite stops proving anything.
		return 1
	}

	defer func() {
		if cerr := db.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "integration harness teardown (postgres): %v\n", cerr)
		}
	}()

	redisServer, err := redistest.Start(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration harness (redis): %v\n", err)

		return 1
	}

	defer func() {
		if cerr := redisServer.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "integration harness teardown (redis): %v\n", cerr)
		}
	}()

	testDB = db
	testRedis = redisServer

	return m.Run()
}

const (
	integrationSecret = "integration-signing-secret-32-bytes!!"
	integrationPass   = "correct horse battery staple"
)

// server is a running API with real dependencies behind it.
type server struct {
	url    string
	store  *store.Store
	logs   *bytes.Buffer
	client *http.Client
}

// newServer builds the router the way cmd/api does, with the login budgets a
// test asks for.
//
// wrapAuthStore, when supplied, sits between the auth service and the real
// store. Exactly one test uses it — the one that has to make registration's
// second transaction fail on purpose (issue #34) — and it is a parameter rather
// than a field on the service because the seam being exercised *is* the boundary
// between the auth service and the store. The router's own store stays the real
// one, so only the flow under test can be interfered with.
func newServer(t *testing.T, limits auth.RateLimitConfig, wrapAuthStore ...func(*store.Store) auth.Store) *server {
	t.Helper()

	if len(wrapAuthStore) > 1 {
		t.Fatalf("newServer takes at most one auth-store wrapper, got %d", len(wrapAuthStore))
	}

	logs := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dataStore := store.New(testDB.AppPool(t, 6))
	kv := auth.NewRedisKeyValue(testRedis.Client(t, int(redisDB.Add(1))%16))

	issuer, err := auth.NewIssuer(auth.TokenConfig{
		Secret:    []byte(integrationSecret),
		Issuer:    "collabboard",
		Audience:  "collabboard-api",
		AccessTTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("building the issuer: %v", err)
	}

	var authStore auth.Store = dataStore
	if len(wrapAuthStore) == 1 {
		authStore = wrapAuthStore[0](dataStore)
	}

	service, err := auth.NewService(auth.ServiceDeps{
		Store:      authStore,
		Deriver:    auth.NewArgon2Deriver(4),
		Issuer:     issuer,
		Sessions:   auth.NewSessionStore(kv, time.Hour),
		Limiter:    auth.NewLimiter(kv, limits, []byte("integration-pepper"), logger),
		Logger:     logger,
		Params:     auth.DefaultArgon2Params(),
		AbsentSalt: []byte("integration-absent-salt-16+"),
	})
	if err != nil {
		t.Fatalf("building the auth service: %v", err)
	}

	router := api.NewRouter(logger,
		api.BodyLimits{},
		api.HealthDeps{},
		api.AuthDeps{Service: service, Verifier: issuer, Store: dataStore},
		api.RealtimeDeps{})

	httpServer := httptest.NewServer(router)
	t.Cleanup(httpServer.Close)

	return &server{url: httpServer.URL, store: dataStore, logs: logs, client: httpServer.Client()}
}

func generousLimits() auth.RateLimitConfig {
	return auth.RateLimitConfig{PerAccount: 1000, PerAddress: 1000, Window: time.Minute}
}

// response is a decoded API reply.
type response struct {
	status int
	body   map[string]any
	raw    string
	header http.Header
}

func (s *server) do(t *testing.T, method, path, token string, body any) response {
	t.Helper()

	return s.send(t, s.request(t, method, path, token, body))
}

// request builds the request do would send, for the tests that have to add a
// header of their own before it goes — which on this surface means the ones
// attacking the tenant boundary.
func (s *server) request(t *testing.T, method, path, token string, body any) *http.Request {
	t.Helper()

	var payload io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the request body: %v", err)
		}

		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, s.url+path, payload)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	return req
}

func (s *server) send(t *testing.T, req *http.Request) response {
	t.Helper()

	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}

	out := response{status: resp.StatusCode, raw: string(raw), header: resp.Header}

	if len(raw) > 0 {
		// A body that is not JSON is not fatal — 404 from the router is plain
		// text — but it must not silently look like an empty object.
		_ = json.Unmarshal(raw, &out.body)
	}

	return out
}

// account is a registered user and its session.
type account struct {
	email        string
	userID       uuid.UUID
	tenantID     uuid.UUID
	accessToken  string
	refreshToken string
}

// register creates an account through the real endpoint and removes it when the
// test ends.
//
// Cleanup goes through the owner pool because deleting a user is exactly what
// the policies forbid: the pre-tenant path has no delete by design (ADR 0002).
func (s *server) register(t *testing.T, label string) account {
	t.Helper()

	email := label + "-" + uuid.NewString()[:8] + "@example.com"

	resp := s.do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email":             email,
		"password":          integrationPass,
		"display_name":      label + " person",
		"organization_name": label + " workspace",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("register: status %d, body %s", resp.status, resp.raw)
	}

	userID := uuid.MustParse(stringField(t, resp.body, "user_id"))

	organization, ok := resp.body["organization"].(map[string]any)
	if !ok {
		t.Fatalf("register response has no organization: %s", resp.raw)
	}

	tenantID := uuid.MustParse(stringField(t, organization, "id"))

	t.Cleanup(func() {
		owner := testDB.OwnerPool(t, 2)

		if _, err := owner.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, tenantID); err != nil {
			t.Errorf("cleaning up organization %s: %v", tenantID, err)
		}

		if _, err := owner.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID); err != nil {
			t.Errorf("cleaning up user %s: %v", userID, err)
		}
	})

	login := s.login(t, email, integrationPass)
	login.email = email
	login.userID = userID
	login.tenantID = tenantID

	return login
}

func (s *server) login(t *testing.T, email, password string) account {
	t.Helper()

	resp := s.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{
		"email":    email,
		"password": password,
	})
	if resp.status != http.StatusOK {
		t.Fatalf("login: status %d, body %s", resp.status, resp.raw)
	}

	return account{
		accessToken:  stringField(t, resp.body, "access_token"),
		refreshToken: stringField(t, resp.body, "refresh_token"),
	}
}

// stringField reads a string out of a decoded body, failing the test rather
// than panicking when the shape is not what the caller expected. A type
// assertion here would report a response-shape regression as a panic in a
// helper, three frames from the test that cares.
func stringField(t *testing.T, body map[string]any, name string) string {
	t.Helper()

	value, ok := body[name].(string)
	if !ok {
		t.Fatalf("response has no string field %q: %v", name, body)
	}

	return value
}

// TestRegisterLoginAndAuthenticatedRequestEndToEnd is acceptance criterion 1,
// against a real database.
func TestRegisterLoginAndAuthenticatedRequestEndToEnd(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")

	t.Logf("registered %s as user %s in organization %s", alice.email, alice.userID, alice.tenantID)

	me := s.do(t, http.MethodGet, "/api/v1/me", alice.accessToken, nil)

	t.Logf("GET /me -> %d %s", me.status, me.raw)

	if me.status != http.StatusOK {
		t.Fatalf("GET /me: status %d, body %s", me.status, me.raw)
	}

	if me.body["user_id"] != alice.userID.String() {
		t.Errorf("/me user_id = %v, want %s", me.body["user_id"], alice.userID)
	}

	organization, ok := me.body["organization"].(map[string]any)
	if !ok || organization["id"] != alice.tenantID.String() {
		t.Errorf("/me organization = %v, want id %s", me.body["organization"], alice.tenantID)
	}

	// The claim that matters: the tenant travelled from the token into
	// store.WithTenant and came back as rows from that tenant, with no handler
	// choosing it.
	members := s.do(t, http.MethodGet, "/api/v1/members", alice.accessToken, nil)

	t.Logf("GET /members -> %d %s", members.status, members.raw)

	if members.status != http.StatusOK {
		t.Fatalf("GET /members: status %d, body %s", members.status, members.raw)
	}

	if !bytes.Contains([]byte(members.raw), []byte(alice.email)) {
		t.Errorf("the member list does not contain the registering user: %s", members.raw)
	}
}

// TestTenantContextFlowsFromTheTokenAndOnlyFromTheToken is the BOLA test
// against a real database.
//
// The recording fake in auth_bola_test.go can assert "no tenant context was
// opened for B". Here the assertion is the one a customer would care about:
// the rows that come back are the caller's, whatever the request says.
func TestTenantContextFlowsFromTheTokenAndOnlyFromTheToken(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	t.Logf("alice: user=%s tenant=%s", alice.userID, alice.tenantID)
	t.Logf("bob:   user=%s tenant=%s", bob.userID, bob.tenantID)

	// The control, in both directions. Each sees exactly their own member list.
	for _, subject := range []struct {
		name  string
		self  account
		other account
	}{
		{name: "alice", self: alice, other: bob},
		{name: "bob", self: bob, other: alice},
	} {
		t.Run("control: "+subject.name+" sees only their own organization", func(t *testing.T) {
			resp := s.do(t, http.MethodGet, "/api/v1/members", subject.self.accessToken, nil)

			t.Logf("%s -> %d %s", subject.name, resp.status, resp.raw)

			if resp.status != http.StatusOK {
				t.Fatalf("status %d, body %s", resp.status, resp.raw)
			}

			if !bytes.Contains([]byte(resp.raw), []byte(subject.self.email)) {
				t.Errorf("%s cannot see their own membership", subject.name)
			}

			if bytes.Contains([]byte(resp.raw), []byte(subject.other.email)) {
				t.Errorf("%s can see %s's organization", subject.name, subject.other.email)
			}
		})
	}

	// The attacks. Alice tries to name bob's tenant through every channel the
	// HTTP surface makes plausible.
	for _, attack := range []struct {
		name   string
		path   string
		header string
	}{
		{name: "X-Tenant-ID header", path: "/api/v1/members", header: "X-Tenant-ID"},
		{name: "X-Organization-ID header", path: "/api/v1/members", header: "X-Organization-ID"},
		{name: "org query parameter", path: "/api/v1/members?org=" + bob.tenantID.String()},
		{name: "tenant_id query parameter", path: "/api/v1/members?tenant_id=" + bob.tenantID.String()},
	} {
		t.Run(attack.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.url+attack.path, nil)
			if err != nil {
				t.Fatalf("building the request: %v", err)
			}

			req.Header.Set("Authorization", "Bearer "+alice.accessToken)

			if attack.header != "" {
				req.Header.Set(attack.header, bob.tenantID.String())
			}

			resp := s.send(t, req)

			t.Logf("alice, %s -> %d %s", attack.name, resp.status, resp.raw)

			if bytes.Contains([]byte(resp.raw), []byte(bob.email)) {
				t.Errorf("BOLA: alice read bob's organization\n%s", resp.raw)
			}

			if resp.status == http.StatusOK && !bytes.Contains([]byte(resp.raw), []byte(alice.email)) {
				t.Errorf("the request succeeded but returned neither tenant's data: %s", resp.raw)
			}
		})
	}

	// And the endpoint that legitimately takes an organization id.
	t.Run("switching into an organization she is not a member of", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/api/v1/auth/organization", alice.accessToken,
			map[string]string{"organization_id": bob.tenantID.String()})

		t.Logf("alice switching into bob's organization -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", resp.status)
		}

		if _, issued := resp.body["access_token"]; issued {
			t.Fatal("a token was issued for an organization alice does not belong to")
		}
	})
}

// TestSwitchingIntoAJoinedOrganizationWorks is the control for the refusal
// above: without it, the 403 would also be produced by an endpoint that refuses
// everything.
//
// The second membership is created the way an invite would create it — a
// tenant-scoped INSERT in bob's organization — so this exercises the real
// mechanism rather than a fixture.
func TestSwitchingIntoAJoinedOrganizationWorks(t *testing.T) {
	ctx := context.Background()
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	if err := s.store.WithTenant(ctx, bob.tenantID, func(ctx context.Context, q store.Querier) error {
		_, err := q.CreateMembership(ctx, store.CreateMembershipParams{UserID: alice.userID, Role: "member"})

		return err
	}); err != nil {
		t.Fatalf("adding alice to bob's organization: %v", err)
	}

	resp := s.do(t, http.MethodPost, "/api/v1/auth/organization", alice.accessToken,
		map[string]string{"organization_id": bob.tenantID.String()})

	t.Logf("alice switching into an organization she now belongs to -> %d %s", resp.status, resp.raw)

	if resp.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.status, resp.raw)
	}

	switched, ok := resp.body["access_token"].(string)
	if !ok || switched == "" {
		t.Fatal("no access token in the switch response")
	}

	// The new token names bob's tenant, and the data it reaches is bob's.
	members := s.do(t, http.MethodGet, "/api/v1/members", switched, nil)

	t.Logf("with the switched token, GET /members -> %d %s", members.status, members.raw)

	if !bytes.Contains([]byte(members.raw), []byte(bob.email)) {
		t.Errorf("the switched token does not reach the organization it names: %s", members.raw)
	}
}

// TestRejectionCases covers the acceptance criterion "requests with
// no/expired/tampered tokens are rejected with correct status codes", plus the
// revoked refresh token.
func TestRejectionCases(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	t.Run("no token", func(t *testing.T) {
		resp := s.do(t, http.MethodGet, "/api/v1/members", "", nil)

		t.Logf("no token -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.status)
		}
	})

	t.Run("tampered signature", func(t *testing.T) {
		resp := s.do(t, http.MethodGet, "/api/v1/members", flipSignature(alice.accessToken), nil)

		t.Logf("tampered signature -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.status)
		}
	})

	t.Run("expired token", func(t *testing.T) {
		expired := expiredTokenFor(t, alice)

		resp := s.do(t, http.MethodGet, "/api/v1/members", expired, nil)

		t.Logf("expired token -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.status)
		}
	})

	t.Run("unknown refresh token", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/api/v1/auth/refresh", "",
			map[string]string{"refresh_token": uuid.NewString()})

		t.Logf("unknown refresh token -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.status)
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/api/v1/auth/login", "",
			map[string]string{"email": alice.email, "password": "not the password at all"})

		t.Logf("wrong password -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.status)
		}

		if resp.body["error"] != "invalid email or password" {
			t.Errorf("body = %s; must not distinguish a wrong password from an unknown address", resp.raw)
		}
	})

	t.Run("unknown address gets the identical answer", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/api/v1/auth/login", "",
			map[string]string{"email": "nobody-" + uuid.NewString() + "@example.com", "password": integrationPass})

		t.Logf("unknown address -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.status)
		}

		if resp.body["error"] != "invalid email or password" {
			t.Errorf("body = %s; must be identical to the wrong-password answer", resp.raw)
		}
	})

	t.Run("duplicate registration", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
			"email": alice.email, "password": integrationPass, "display_name": "Impostor",
		})

		t.Logf("duplicate registration -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusConflict {
			t.Errorf("status = %d, want 409", resp.status)
		}
	})
}

// TestRefreshRotationAndRevocation is the "refresh token revocation works"
// criterion, against a real Redis.
func TestRefreshRotationAndRevocation(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	first := s.do(t, http.MethodPost, "/api/v1/auth/refresh", "",
		map[string]string{"refresh_token": alice.refreshToken})
	if first.status != http.StatusOK {
		t.Fatalf("refresh: status %d, body %s", first.status, first.raw)
	}

	rotated := stringField(t, first.body, "refresh_token")

	t.Logf("refresh rotated the token: %t", rotated != alice.refreshToken)

	if rotated == alice.refreshToken {
		t.Error("refresh returned the same refresh token")
	}

	// The new access token works.
	me := s.do(t, http.MethodGet, "/api/v1/me", stringField(t, first.body, "access_token"), nil)
	if me.status != http.StatusOK {
		t.Errorf("the refreshed access token does not work: %d %s", me.status, me.raw)
	}

	// Replaying the original is reuse: it must be refused *and* it must take
	// the session with it.
	replay := s.do(t, http.MethodPost, "/api/v1/auth/refresh", "",
		map[string]string{"refresh_token": alice.refreshToken})

	t.Logf("replaying the rotated-away token -> %d %s", replay.status, replay.raw)

	if replay.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", replay.status)
	}

	afterReuse := s.do(t, http.MethodPost, "/api/v1/auth/refresh", "",
		map[string]string{"refresh_token": rotated})

	t.Logf("the token that was live when the replay happened -> %d %s", afterReuse.status, afterReuse.raw)

	if afterReuse.status != http.StatusUnauthorized {
		t.Errorf("the live token still works after reuse detection: %d", afterReuse.status)
	}

	// And explicit logout, on a fresh session.
	second := s.login(t, alice.email, integrationPass)

	logout := s.do(t, http.MethodPost, "/api/v1/auth/logout", "",
		map[string]string{"refresh_token": second.refreshToken})

	t.Logf("logout -> %d", logout.status)

	if logout.status != http.StatusNoContent {
		t.Errorf("logout status = %d, want 204", logout.status)
	}

	revoked := s.do(t, http.MethodPost, "/api/v1/auth/refresh", "",
		map[string]string{"refresh_token": second.refreshToken})

	t.Logf("refreshing a revoked token -> %d %s", revoked.status, revoked.raw)

	if revoked.status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", revoked.status)
	}
}

// TestLoginIsRateLimited checks the 429 and the Retry-After header a client
// needs to back off.
func TestLoginIsRateLimited(t *testing.T) {
	s := newServer(t, auth.RateLimitConfig{PerAccount: 3, PerAddress: 1000, Window: time.Minute})
	alice := s.register(t, "alice")

	var limited response

	for attempt := range 8 {
		resp := s.do(t, http.MethodPost, "/api/v1/auth/login", "",
			map[string]string{"email": alice.email, "password": "wrong password entirely"})
		if resp.status == http.StatusTooManyRequests {
			limited = resp

			t.Logf("limited on attempt %d: %d %s", attempt+1, resp.status, resp.raw)

			break
		}
	}

	if limited.status != http.StatusTooManyRequests {
		t.Fatal("eight attempts against a budget of three were all allowed")
	}

	retryAfter := limited.header.Get("Retry-After")

	t.Logf("Retry-After: %q", retryAfter)

	seconds, err := strconv.Atoi(retryAfter)
	if err != nil || seconds <= 0 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", retryAfter)
	}
}

// TestLoginTakesComparableTimeForAnUnknownAddress is the wall-clock backstop
// behind the derivation-counting test in internal/auth.
//
// The bound is deliberately loose. The counting test is the precise claim; this
// one exists to catch the case where some *other* step becomes asymmetric — an
// extra database round trip, a skipped Redis write — which counting derivations
// would not notice. A tight bound here would flake on a shared runner and teach
// everyone to re-run it.
func TestLoginTakesComparableTimeForAnUnknownAddress(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	const samples = 5

	measure := func(email string) time.Duration {
		var total time.Duration

		for range samples {
			start := time.Now()

			resp := s.do(t, http.MethodPost, "/api/v1/auth/login", "",
				map[string]string{"email": email, "password": "definitely the wrong password"})

			total += time.Since(start)

			if resp.status != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d: %s", resp.status, resp.raw)
			}
		}

		return total / samples
	}

	known := measure(alice.email)
	unknown := measure("nobody-" + uuid.NewString() + "@example.com")

	ratio := float64(unknown) / float64(known)

	t.Logf("mean login time over %d samples: known address %s, unknown address %s (ratio %.2f)",
		samples, known.Round(time.Millisecond), unknown.Round(time.Millisecond), ratio)

	// A skipped argon2id derivation would put the unknown case at a small
	// fraction of the known one; a stand-in that did *more* work would put it
	// well above. Either would be a usable oracle over a network.
	if ratio < 0.5 || ratio > 2.0 {
		t.Errorf("an unknown address takes %.2fx the time of a known one; the difference is an enumeration oracle", ratio)
	}
}

// TestNoTokenOrPasswordMaterialReachesTheLogs is requirement 7, checked against
// the log the server actually emitted during a full flow rather than against a
// reading of the source.
func TestNoTokenOrPasswordMaterialReachesTheLogs(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	s.do(t, http.MethodGet, "/api/v1/me", alice.accessToken, nil)
	s.do(t, http.MethodPost, "/api/v1/auth/login", "",
		map[string]string{"email": alice.email, "password": "wrong"})
	s.do(t, http.MethodPost, "/api/v1/auth/refresh", "",
		map[string]string{"refresh_token": alice.refreshToken})

	output := s.logs.String()

	t.Logf("%d bytes of server log emitted during the flow", len(output))

	for _, secret := range []struct{ name, value string }{
		{name: "the password", value: integrationPass},
		{name: "the access token", value: alice.accessToken},
		{name: "the refresh token", value: alice.refreshToken},
		{name: "the email address", value: alice.email},
	} {
		if secret.value == "" {
			t.Fatalf("%s is empty; this test would pass vacuously", secret.name)
		}

		if bytes.Contains([]byte(output), []byte(secret.value)) {
			t.Errorf("%s appears in the server logs", secret.name)
		}
	}

	for _, want := range []string{"auth.register.success", "auth.login.failed", "auth.refresh.success"} {
		if !bytes.Contains([]byte(output), []byte(want)) {
			t.Errorf("the logs do not contain %q; the assertions above would hold for a silent logger", want)
		}
	}
}

// expiredTokenFor mints a token that is correct in every way except the hour on
// the clock: signed with the server's real key, right issuer, right audience,
// right claims. Built by hand rather than by sleeping past a short ttl, which
// would put a real delay in the suite.
func expiredTokenFor(t *testing.T, a account) string {
	t.Helper()

	issued := time.Now().Add(-time.Hour)

	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   a.userID.String(),
			Issuer:    "collabboard",
			Audience:  jwt.ClaimStrings{"collabboard-api"},
			IssuedAt:  jwt.NewNumericDate(issued),
			NotBefore: jwt.NewNumericDate(issued),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			ID:        uuid.NewString(),
		},
		OrganizationID: a.tenantID.String(),
		Role:           "owner",
		SessionID:      uuid.NewString(),
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(integrationSecret))
	if err != nil {
		t.Fatalf("signing an expired token: %v", err)
	}

	return signed
}

func flipSignature(token string) string {
	parts := bytes.Split([]byte(token), []byte("."))
	if len(parts) != 3 || len(parts[2]) < 4 {
		return token
	}

	mid := len(parts[2]) / 2
	if parts[2][mid] == 'A' {
		parts[2][mid] = 'B'
	} else {
		parts[2][mid] = 'A'
	}

	return string(bytes.Join(parts, []byte(".")))
}
