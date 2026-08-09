package api

// The request body limit.
//
// Every test here is written so that it fails against a limit that is silently
// infinite. That is the whole risk with a bound like this one: a middleware that
// wraps the body and never trips looks exactly like a middleware that works,
// until the day someone sends a gigabyte. So the boundary tests assert on both
// sides of the limit, and the two "not read" tests assert on the number of bytes
// the server actually took off the socket rather than on the status code alone.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf16"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/config"
)

// TestTheFallbackLimitsMatchTheConfiguredDefaults pins the one duplication in
// this design.
//
// internal/api cannot import internal/config — config is a leaf and the
// dependency would run the wrong way — so the numbers exist twice: once as the
// operator-facing default in config, once as the value a zero [BodyLimits]
// resolves to. Two copies of a security limit that can drift apart silently is
// worth exactly one test.
func TestTheFallbackLimitsMatchTheConfiguredDefaults(t *testing.T) {
	t.Parallel()

	if fallbackMaxRequestBytes != config.DefaultMaxRequestBytes {
		t.Errorf("fallback default = %d, config default = %d",
			fallbackMaxRequestBytes, config.DefaultMaxRequestBytes)
	}

	if fallbackMaxUnauthenticatedRequestBytes != config.DefaultMaxUnauthenticatedRequestBytes {
		t.Errorf("fallback unauthenticated = %d, config unauthenticated = %d",
			fallbackMaxUnauthenticatedRequestBytes, config.DefaultMaxUnauthenticatedRequestBytes)
	}
}

// TestAnUnsetLimitIsADefaultAndNotUnlimited covers the resolution rules
// directly, because every other test in this file builds a router through them.
func TestAnUnsetLimitIsADefaultAndNotUnlimited(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                    string
		in                      BodyLimits
		wantDefault, wantUnauth int64
	}{
		{
			name:        "unset",
			in:          BodyLimits{},
			wantDefault: fallbackMaxRequestBytes,
			wantUnauth:  fallbackMaxUnauthenticatedRequestBytes,
		},
		{
			name:        "zero is not unlimited",
			in:          BodyLimits{Default: 0, Unauthenticated: 0},
			wantDefault: fallbackMaxRequestBytes,
			wantUnauth:  fallbackMaxUnauthenticatedRequestBytes,
		},
		{
			name:        "negative is not unlimited either",
			in:          BodyLimits{Default: -1, Unauthenticated: -1},
			wantDefault: fallbackMaxRequestBytes,
			wantUnauth:  fallbackMaxUnauthenticatedRequestBytes,
		},
		{
			name:        "a global limit under the unauthenticated default pulls it down",
			in:          BodyLimits{Default: 1024},
			wantDefault: 1024,
			wantUnauth:  1024,
		},
		{
			name:        "an unauthenticated limit looser than the global one would never apply",
			in:          BodyLimits{Default: 2048, Unauthenticated: 4096},
			wantDefault: 2048,
			wantUnauth:  2048,
		},
		{
			name:        "both configured",
			in:          BodyLimits{Default: 4096, Unauthenticated: 512},
			wantDefault: 4096,
			wantUnauth:  512,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := tc.in.resolved()

			if got.Default != tc.wantDefault || got.Unauthenticated != tc.wantUnauth {
				t.Errorf("%+v resolved to %+v, want default %d and unauthenticated %d",
					tc.in, got, tc.wantDefault, tc.wantUnauth)
			}
		})
	}
}

// bodyLimitFixture is a fully wired router with the limits under test, plus the
// two things the assertions need: whether the auth service was reached, and a
// token for the authenticated routes.
type bodyLimitFixture struct {
	router  *gin.Engine
	service *countingAuthService
	objects *tenantObjects
	token   string
}

func newBodyLimitFixture(t *testing.T, limits BodyLimits) *bodyLimitFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	issuer := testIssuer(t)
	tenantStore := newCRUDStore()
	tenantID := uuid.New()
	service := &countingAuthService{}

	router := NewRouter(discardLogger(), limits,
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
		AuthDeps{Service: service, Verifier: issuer, Store: tenantStore},
		RealtimeDeps{Publisher: &recordingPublisher{}})

	token, _, err := issuer.Issue(auth.Principal{
		UserID: uuid.New(), TenantID: tenantID, Role: "owner", SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing a token: %v", err)
	}

	return &bodyLimitFixture{
		router:  router,
		service: service,
		objects: tenantStore.seed(tenantID, "alpha"),
		token:   token,
	}
}

// countingAuthService answers every credential with "no", and counts. The count
// is the assertion that matters: a body refused for its size must not have
// reached the service at all, and a body under the limit must have.
type countingAuthService struct {
	logins atomic.Int64
}

func (s *countingAuthService) Register(context.Context, auth.RegisterInput) (auth.RegisterResult, error) {
	return auth.RegisterResult{}, auth.ErrInvalidCredentials
}

func (s *countingAuthService) Login(context.Context, auth.LoginInput) (auth.LoginResult, error) {
	s.logins.Add(1)

	return auth.LoginResult{}, auth.ErrInvalidCredentials
}

func (s *countingAuthService) Refresh(context.Context, string) (auth.LoginResult, error) {
	return auth.LoginResult{}, auth.ErrRefreshUnknown
}

func (s *countingAuthService) Logout(context.Context, string) error { return nil }

func (s *countingAuthService) SwitchOrganization(
	context.Context, auth.Principal, uuid.UUID,
) (auth.LoginResult, error) {
	return auth.LoginResult{}, auth.ErrNotAMember
}

func (s *countingAuthService) Me(context.Context, auth.Principal) (auth.MeResult, error) {
	return auth.MeResult{}, nil
}

func (s *countingAuthService) AddMember(context.Context, auth.AddMemberInput) (auth.AddMemberResult, error) {
	return auth.AddMemberResult{}, auth.ErrNoSuchAccount
}

func (s *countingAuthService) CreateFirstOrganization(
	context.Context, auth.CreateOrganizationInput,
) (auth.CreateOrganizationResult, error) {
	return auth.CreateOrganizationResult{}, auth.ErrInvalidCredentials
}

// jsonOfExactly renders prefix + padding + suffix at exactly size bytes, so a
// test can ask for a body one byte either side of a limit and mean it.
func jsonOfExactly(t *testing.T, size int, prefix, suffix string) string {
	t.Helper()

	padding := size - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("cannot build a %d byte body out of a %d byte template", size, len(prefix)+len(suffix))
	}

	return prefix + strings.Repeat("a", padding) + suffix
}

// TestOneByteOverTheLimitIsRefusedAndOneByteUnderIsNot is the acceptance
// criterion from #50, on both surfaces.
//
// Small configured limits rather than the real defaults, because the claim is
// about the boundary and a 256 KiB boundary costs 256 KiB per subtest to probe.
// The real defaults are exercised against a real maximal body in
// TestTheLargestLegitimateBodyIsAccepted below.
func TestOneByteOverTheLimitIsRefusedAndOneByteUnderIsNot(t *testing.T) {
	t.Parallel()

	const (
		globalLimit = 1024
		authLimit   = 512
	)

	for _, tc := range []struct {
		name  string
		limit int
		path  string

		// prefix and suffix bracket the padding that makes the body its exact
		// size. Both bodies are valid JSON for their endpoint at every size.
		prefix, suffix string

		authenticate bool

		// wantUnder is the status a body at exactly the limit produces once it
		// reaches the handler. It is deliberately not 2xx everywhere: the point
		// is that the request was decoded and answered on its merits.
		wantUnder int
	}{
		{
			name:      "an unauthenticated login, on the tighter limit",
			limit:     authLimit,
			path:      "/api/v1/auth/login",
			prefix:    `{"email":"someone@example.com","password":"`,
			suffix:    `"}`,
			wantUnder: http.StatusUnauthorized,
		},
		{
			name:         "an authenticated card create, on the global limit",
			limit:        globalLimit,
			path:         "/api/v1/columns/%s/cards",
			prefix:       `{"title":"a card","description":"`,
			suffix:       `"}`,
			authenticate: true,
			wantUnder:    http.StatusCreated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newBodyLimitFixture(t, BodyLimits{Default: globalLimit, Unauthenticated: authLimit})

			path := tc.path
			if tc.authenticate {
				path = fmt.Sprintf(tc.path, f.objects.column.ID)
			}

			call := func(size int) *httptest.ResponseRecorder {
				body := jsonOfExactly(t, size, tc.prefix, tc.suffix)

				rec := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")

				if tc.authenticate {
					req.Header.Set("Authorization", "Bearer "+f.token)
				}

				if req.ContentLength != int64(size) {
					t.Fatalf("built a %d byte body but the request declares %d", size, req.ContentLength)
				}

				f.router.ServeHTTP(rec, req)

				return rec
			}

			under := call(tc.limit)
			t.Logf("a body of exactly %d bytes -> %d %s", tc.limit, under.Code, under.Body.String())

			if under.Code != tc.wantUnder {
				t.Errorf("a body at exactly the limit -> %d, want %d: the limit is refusing legitimate requests",
					under.Code, tc.wantUnder)
			}

			over := call(tc.limit + 1)
			t.Logf("a body of exactly %d bytes -> %d %s", tc.limit+1, over.Code, over.Body.String())

			if over.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("a body one byte over the limit -> %d, want 413: the limit is not being enforced", over.Code)
			}

			assertTooLargeBody(t, over.Body.Bytes())
		})
	}
}

// TestAnOversizedBodyNeverReachesTheAuthService is the security claim stated
// directly: the refusal happens before authentication, not after it.
func TestAnOversizedBodyNeverReachesTheAuthService(t *testing.T) {
	t.Parallel()

	f := newBodyLimitFixture(t, BodyLimits{Default: 4096, Unauthenticated: 512})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(jsonOfExactly(t, 4096, `{"email":"a@example.com","password":"`, `"}`)))
	req.Header.Set("Content-Type", "application/json")
	f.router.ServeHTTP(rec, req)

	t.Logf("an oversized login -> %d %s", rec.Code, rec.Body.String())

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}

	if got := f.service.logins.Load(); got != 0 {
		t.Errorf("the auth service saw %d logins; an oversized body must be refused before authentication", got)
	}
}

// TestTheLargestLegitimateBodyIsAccepted finds the real boundary — the biggest
// body the field limits permit — and tests both sides of it against the real
// default limits.
//
// The description is built out of astral characters so that every rune costs the
// maximum a rune can cost: Go's encoder writes them literally at 4 bytes, and a
// client that escapes them writes a surrogate pair at 12. Both are covered, and
// the escaped form is the one that decides whether 256 KiB is enough.
func TestTheLargestLegitimateBodyIsAccepted(t *testing.T) {
	t.Parallel()

	f := newBodyLimitFixture(t, BodyLimits{})
	path := "/api/v1/columns/" + f.objects.column.ID.String() + "/cards"

	post := func(t *testing.T, body string) *httptest.ResponseRecorder {
		t.Helper()

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+f.token)
		f.router.ServeHTTP(rec, req)

		return rec
	}

	// worstCaseRune is one astral character written the most expensive way JSON
	// allows: an escaped surrogate pair, twelve ASCII bytes for one rune. It is
	// built rather than typed so that no editor or terminal between here and the
	// test run can normalise it into something cheaper.
	//
	// This is the number the 256 KiB limit was chosen against, and it is a
	// legitimate body rather than padding: encoding/json decodes it straight
	// back to U+1F600, so the field validators count exactly the runes the
	// handler is allowed.
	high, low := utf16.EncodeRune('\U0001F600')
	worstCaseRune := fmt.Sprintf(`\u%04X\u%04X`, high, low)

	// The largest card the handlers will accept: the title at maxNameLength
	// runes and the description at maxDescriptionLength.
	maximal := func(t *testing.T, escaped bool, descriptionRunes int) string {
		t.Helper()

		encode := func(runes int) string {
			if escaped {
				return `"` + strings.Repeat(worstCaseRune, runes) + `"`
			}

			// The same runes as Go would send them: literal UTF-8, four bytes.
			body, err := json.Marshal(strings.Repeat("\U0001F600", runes))
			if err != nil {
				t.Fatalf("encoding: %v", err)
			}

			return string(body)
		}

		return `{"title":` + encode(maxNameLength) + `,"description":` + encode(descriptionRunes) + `}`
	}

	for _, escaped := range []bool{false, true} {
		body := maximal(t, escaped, maxDescriptionLength)

		rec := post(t, body)
		t.Logf("the largest legitimate card (escaped=%t) is %d bytes of a %d byte limit -> %d",
			escaped, len(body), fallbackMaxRequestBytes, rec.Code)

		if len(body) > fallbackMaxRequestBytes {
			t.Fatalf("the largest legitimate body is %d bytes, over the %d byte limit: the limit is too small",
				len(body), fallbackMaxRequestBytes)
		}

		if rec.Code != http.StatusCreated {
			t.Fatalf("the largest legitimate card -> %d %s, want 201", rec.Code, rec.Body.String())
		}
	}

	// One rune past what the field permits is still well under the body limit,
	// and must be refused by the field check with its own message. This is the
	// half of the boundary that proves the body limit did not quietly replace
	// the validation underneath it.
	overField := maximal(t, true, maxDescriptionLength+1)

	rec := post(t, overField)
	t.Logf("one rune past the description limit (%d bytes) -> %d %s", len(overField), rec.Code, rec.Body.String())

	if rec.Code != http.StatusBadRequest {
		t.Errorf("a description one rune too long -> %d, want 400", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "description is too long") {
		t.Errorf("the field limit stopped saying what it refused: %s", rec.Body.String())
	}

	// And one byte past the body limit is the body limit's answer, not the
	// field's — the two refusals are distinguishable.
	oversized := jsonOfExactly(t, fallbackMaxRequestBytes+1, `{"title":"a card","description":"`, `"}`)

	rec = post(t, oversized)
	t.Logf("one byte past the body limit (%d bytes) -> %d %s", len(oversized), rec.Code, rec.Body.String())

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a body one byte over the default limit -> %d, want 413", rec.Code)
	}
}

// assertTooLargeBody checks that a 413 looks like every other refusal this API
// writes: one field, no detail.
func assertTooLargeBody(t *testing.T, body []byte) {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("the 413 body is not JSON: %v (%s)", err, body)
	}

	if len(decoded) != 1 || decoded["error"] != messageBodyTooLarge {
		t.Errorf("413 body = %s, want exactly {\"error\":%q}", body, messageBodyTooLarge)
	}
}
