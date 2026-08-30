package api

// POST /api/v1/me/organizations at the HTTP layer (issue #86).
//
// The service's own rules are tested in internal/auth. What is left here is the
// adaptation: which status each refusal becomes, and that the route is on the
// authenticated group. The last of those is already asserted for free by
// router_test.go's inventory, which probes every mounted route without a token
// and requires a 401 from anything not on its short public list — a route added
// to the wrong group fails that test rather than this one.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// capRouter builds a router whose auth service always reports the cap.
//
// countingAuthService's CreateAdditionalOrganization returns
// auth.ErrTooManyOrganizations, which is the one refusal this route introduces
// and therefore the one worth adapting a test around.
func capRouter(t *testing.T) (*gin.Engine, string) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	issuer := testIssuer(t)

	router := NewRouter(discardLogger(), BodyLimits{}, nil,
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
		AuthDeps{Service: &countingAuthService{}, Verifier: issuer},
		RealtimeDeps{})

	token, _, err := issuer.Issue(auth.Principal{
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		Role:      "owner",
		SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing a token: %v", err)
	}

	return router, token
}

func postWorkspace(t *testing.T, router *gin.Engine, token, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/me/organizations", bytes.NewReader([]byte(body)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec
}

func TestTheWorkspaceCapAnswers409AndNamesTheReason(t *testing.T) {
	t.Parallel()

	router, token := capRouter(t)

	rec := postWorkspace(t, router, token, `{"name":"One Too Many"}`)

	t.Logf("-> %d %s", rec.Code, rec.Body.String())

	// 409 rather than 403, matching ErrAlreadyHasOrganization beside it: the
	// caller is entitled to create workspaces and has created as many as they
	// may. A 403 would say "you may not do this", which is not true.
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}

	var body struct {
		Error string `json:"error"`
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}

	// The message has to be usable by the screen that shows it. "Conflict" with
	// no sentence is a limit the client cannot explain to whoever hit it.
	if body.Error == "" {
		t.Fatal("a 409 with no message; the client has nothing to render")
	}

	// And it must not leak the internal error text, which names the package.
	if bytes.Contains(rec.Body.Bytes(), []byte("auth:")) {
		t.Errorf("the response relays the internal error: %s", rec.Body.String())
	}
}

func TestAWorkspaceNameIsRequiredByTheBinder(t *testing.T) {
	t.Parallel()

	router, token := capRouter(t)

	// `binding:"required"` refuses both, before the service is reached — which
	// is why this is a 400 and not the service's own ErrInvalidInput 400. Both
	// answers are correct; asserting the status rather than the sentence is what
	// keeps that an implementation detail.
	for _, body := range []string{`{}`, `{"name":""}`} {
		rec := postWorkspace(t, router, token, body)

		t.Logf("%s -> %d %s", body, rec.Code, rec.Body.String())

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestCreatingAWorkspaceRequiresAToken(t *testing.T) {
	t.Parallel()

	router, _ := capRouter(t)

	rec := postWorkspace(t, router, "", `{"name":"Anonymous"}`)

	// Belt and braces with router_test.go's inventory, which asserts the same
	// thing across every route. Named here too because "#86 added an
	// authenticated route" is the claim, and a claim worth making is worth an
	// assertion someone can find by grepping for the path.
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
