package api

// The BOLA test for the board surface: an authenticated member of organization A
// cannot read, update, move or delete any object of organization B, by id, on
// any endpoint.
//
// # Why this file exists next to auth_bola_test.go
//
// That file proves the tenant cannot be *chosen* by a request. This one proves
// the consequence for a surface where every URL now carries an object id. Those
// are different claims: a service can take its tenant strictly from the token
// and still leak, if a handler resolves a path id first and scopes second. That
// is the ordinary shape of a broken-object-level-authorization bug, and it is
// the top item on the OWASP API list.
//
// # What is asserted, per endpoint
//
// Every route is exercised twice, with the same fixture and the same token:
//
//   - with alice's own object id — the control, which must succeed and must
//     return alice's data. Without it the cross-tenant assertion would also pass
//     against a router that answered 404 to everything;
//   - with bob's object id — which must not return bob's data, must not echo
//     bob's ids, and must not open a tenant context for bob.
//
// The third of those is the strongest and is the reason the fake records rather
// than merely serves: a handler that opened bob's tenant and then filtered the
// rows in Go would satisfy the first two and be one refactor away from leaking.
//
// TestBoardBOLAAssertionsHaveTeeth at the bottom runs the same attack against a
// deliberately vulnerable router and requires it to succeed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// tenantObjects is one tenant's worth of hierarchy: one row per level, each
// carrying a marker string that appears nowhere else.
//
// One row per level rather than several is deliberate. The question here is
// "can alice reach bob's card by id", which needs exactly one of bob's cards.
type tenantObjects struct {
	marker  string
	project store.Project
	board   store.Board
	column  store.Column
	card    store.Card
}

func newTenantObjects(tenantID uuid.UUID, marker string) *tenantObjects {
	projectID, boardID, columnID, cardID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	return &tenantObjects{
		marker: marker,
		project: store.Project{
			ID: projectID, TenantID: tenantID, Name: marker + "-project", Description: marker,
		},
		board: store.Board{
			ID: boardID, TenantID: tenantID, ProjectID: projectID, Name: marker + "-board",
		},
		column: store.Column{
			ID: columnID, TenantID: tenantID, BoardID: boardID, Name: marker + "-column",
		},
		card: store.Card{
			ID: cardID, TenantID: tenantID, BoardID: boardID, ColumnID: columnID,
			Title: marker + "-card", Description: marker,
		},
	}
}

// crudStore is a TenantStore that records every tenant a transaction was opened
// for and serves exactly one tenant's objects to each.
//
// It is not a database. It models the one behaviour that matters for this test —
// a query scoped to tenant X sees only tenant X's rows, which is what the RLS
// policy does — so a handler that scoped correctly gets its data and a handler
// that scoped wrongly is visible in `opened`.
type crudStore struct {
	mu      sync.Mutex
	opened  []uuid.UUID
	objects map[uuid.UUID]*tenantObjects
}

func newCRUDStore() *crudStore {
	return &crudStore{objects: map[uuid.UUID]*tenantObjects{}}
}

func (s *crudStore) seed(tenantID uuid.UUID, marker string) *tenantObjects {
	s.mu.Lock()
	defer s.mu.Unlock()

	objects := newTenantObjects(tenantID, marker)
	s.objects[tenantID] = objects

	return objects
}

func (s *crudStore) WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error {
	s.mu.Lock()
	s.opened = append(s.opened, tenantID)
	s.mu.Unlock()

	return fn(ctx, crudQuerier{store: s, tenantID: tenantID})
}

func (s *crudStore) openedTenants() []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.opened)
}

// crudQuerier answers every query in the board surface against one tenant's
// objects, and ErrNoRows for anything else — including, and this is the point,
// another tenant's id.
//
// Writes do not mutate: each subtest builds a fresh fixture, and a delete that
// really deleted would only make the control for the *next* assertion depend on
// the order the subtests happened to run in.
type crudQuerier struct {
	store.Querier

	store    *crudStore
	tenantID uuid.UUID
}

func (q crudQuerier) own() (*tenantObjects, bool) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	objects, ok := q.store.objects[q.tenantID]

	return objects, ok
}

func (q crudQuerier) ListProjects(context.Context) ([]store.Project, error) {
	own, ok := q.own()
	if !ok {
		return []store.Project{}, nil
	}

	return []store.Project{own.project}, nil
}

func (q crudQuerier) CreateProject(_ context.Context, arg store.CreateProjectParams) (store.Project, error) {
	return store.Project{ID: uuid.New(), TenantID: q.tenantID, Name: arg.Name, Description: arg.Description}, nil
}

func (q crudQuerier) GetProject(_ context.Context, id uuid.UUID) (store.Project, error) {
	own, ok := q.own()
	if !ok || own.project.ID != id {
		return store.Project{}, store.ErrNoRows
	}

	return own.project, nil
}

func (q crudQuerier) UpdateProject(_ context.Context, arg store.UpdateProjectParams) (store.Project, error) {
	own, ok := q.own()
	if !ok || own.project.ID != arg.ProjectID {
		return store.Project{}, store.ErrNoRows
	}

	return own.project, nil
}

func (q crudQuerier) ArchiveProject(_ context.Context, id uuid.UUID) (store.Project, error) {
	own, ok := q.own()
	if !ok || own.project.ID != id {
		return store.Project{}, store.ErrNoRows
	}

	return own.project, nil
}

func (q crudQuerier) CreateBoard(_ context.Context, arg store.CreateBoardParams) (store.Board, error) {
	own, ok := q.own()
	if !ok || own.project.ID != arg.ProjectID {
		return store.Board{}, store.ErrNoRows
	}

	return own.board, nil
}

func (q crudQuerier) ListBoardsByProject(_ context.Context, id uuid.UUID) ([]store.Board, error) {
	own, ok := q.own()
	if !ok || own.project.ID != id {
		return []store.Board{}, nil
	}

	return []store.Board{own.board}, nil
}

func (q crudQuerier) GetBoard(_ context.Context, id uuid.UUID) (store.Board, error) {
	return q.board(id)
}

func (q crudQuerier) LockBoard(_ context.Context, id uuid.UUID) (store.Board, error) {
	return q.board(id)
}

func (q crudQuerier) board(id uuid.UUID) (store.Board, error) {
	own, ok := q.own()
	if !ok || own.board.ID != id {
		return store.Board{}, store.ErrNoRows
	}

	return own.board, nil
}

func (q crudQuerier) UpdateBoard(_ context.Context, arg store.UpdateBoardParams) (store.Board, error) {
	return q.board(arg.BoardID)
}

// DeleteBoard reports rows affected, which is how the real :execrows query
// tells a caller that the id named nothing it could see. Zero for another
// tenant's board, and — as everywhere in this fake — nothing is actually
// removed.
func (q crudQuerier) DeleteBoard(_ context.Context, id uuid.UUID) (int64, error) {
	own, ok := q.own()
	if !ok || own.board.ID != id {
		return 0, nil
	}

	return 1, nil
}

func (q crudQuerier) CreateColumn(_ context.Context, arg store.CreateColumnParams) (store.Column, error) {
	own, ok := q.own()
	if !ok || own.board.ID != arg.BoardID {
		return store.Column{}, store.ErrNoRows
	}

	return own.column, nil
}

func (q crudQuerier) ListColumnsByBoard(_ context.Context, id uuid.UUID) ([]store.Column, error) {
	own, ok := q.own()
	if !ok || own.board.ID != id {
		return []store.Column{}, nil
	}

	return []store.Column{own.column}, nil
}

func (q crudQuerier) GetColumn(_ context.Context, id uuid.UUID) (store.Column, error) {
	return q.column(id)
}

func (q crudQuerier) LockColumn(_ context.Context, id uuid.UUID) (store.Column, error) {
	return q.column(id)
}

func (q crudQuerier) column(id uuid.UUID) (store.Column, error) {
	own, ok := q.own()
	if !ok || own.column.ID != id {
		return store.Column{}, store.ErrNoRows
	}

	return own.column, nil
}

func (q crudQuerier) UpdateColumn(_ context.Context, arg store.UpdateColumnParams) (store.Column, error) {
	return q.column(arg.ColumnID)
}

// MoveColumn models the same anchor rule as MoveCard, for the same reason.
func (q crudQuerier) MoveColumn(_ context.Context, arg store.MoveColumnParams) (store.MoveColumnRow, error) {
	column, err := q.column(arg.ColumnID)
	if err != nil {
		return store.MoveColumnRow{}, err
	}

	if arg.AfterColumnID != nil {
		return store.MoveColumnRow{}, store.ErrNoRows
	}

	return store.MoveColumnRow{
		ID: column.ID, TenantID: column.TenantID, BoardID: column.BoardID, Name: column.Name,
	}, nil
}

func (q crudQuerier) DeleteColumn(_ context.Context, id uuid.UUID) (int64, error) {
	own, ok := q.own()
	if !ok || own.column.ID != id {
		return 0, nil
	}

	return 1, nil
}

func (q crudQuerier) CreateCard(_ context.Context, arg store.CreateCardParams) (store.Card, error) {
	own, ok := q.own()
	if !ok || own.column.ID != arg.ColumnID {
		return store.Card{}, store.ErrNoRows
	}

	return own.card, nil
}

func (q crudQuerier) ListCardsByColumn(_ context.Context, id uuid.UUID) ([]store.Card, error) {
	own, ok := q.own()
	if !ok || own.column.ID != id {
		return []store.Card{}, nil
	}

	return []store.Card{own.card}, nil
}

func (q crudQuerier) ListCardsByBoard(_ context.Context, id uuid.UUID) ([]store.Card, error) {
	own, ok := q.own()
	if !ok || own.board.ID != id {
		return []store.Card{}, nil
	}

	return []store.Card{own.card}, nil
}

func (q crudQuerier) GetCard(_ context.Context, id uuid.UUID) (store.Card, error) {
	return q.cardByID(id)
}

func (q crudQuerier) cardByID(id uuid.UUID) (store.Card, error) {
	own, ok := q.own()
	if !ok || own.card.ID != id {
		return store.Card{}, store.ErrNoRows
	}

	return own.card, nil
}

func (q crudQuerier) UpdateCard(_ context.Context, arg store.UpdateCardParams) (store.Card, error) {
	return q.cardByID(arg.CardID)
}

// MoveCard models the anchor rule, which is the half of the real query that a
// fake could most easily leave out and most badly mislead by leaving out: an
// after_card_id that is not another card currently in the target column produces
// no row. In this fixture each tenant has exactly one card, so *any* non-null
// anchor fails that test — including the other tenant's, which is the case
// TestMovingOwnCardIntoAnotherTenantsColumnIsRefused is about.
func (q crudQuerier) MoveCard(_ context.Context, arg store.MoveCardParams) (store.MoveCardRow, error) {
	card, err := q.cardByID(arg.CardID)
	if err != nil {
		return store.MoveCardRow{}, err
	}

	own, ok := q.own()
	if !ok || own.column.ID != arg.ColumnID {
		return store.MoveCardRow{}, store.ErrNoRows
	}

	if arg.AfterCardID != nil {
		return store.MoveCardRow{}, store.ErrNoRows
	}

	return store.MoveCardRow{
		ID: card.ID, TenantID: card.TenantID, BoardID: card.BoardID, ColumnID: arg.ColumnID,
		Title: card.Title, Description: card.Description,
	}, nil
}

func (q crudQuerier) DeleteCard(_ context.Context, id uuid.UUID) (int64, error) {
	own, ok := q.own()
	if !ok || own.card.ID != id {
		return 0, nil
	}

	return 1, nil
}

func (q crudQuerier) RebalanceBoardColumns(context.Context, uuid.UUID) error {
	return nil
}

func (q crudQuerier) RebalanceColumnCards(context.Context, uuid.UUID) error {
	return nil
}

// boardFixture is a router, a two-tenant dataset, and alice's token.
type boardFixture struct {
	router *gin.Engine
	store  *crudStore
	issuer *auth.Issuer
	token  string

	tenantA uuid.UUID
	tenantB uuid.UUID
	alice   *tenantObjects
	bob     *tenantObjects
}

func newBoardFixture(t *testing.T) *boardFixture {
	t.Helper()

	gin.SetMode(gin.TestMode)

	issuer := testIssuer(t)
	tenantA, tenantB := uuid.New(), uuid.New()
	aliceID := uuid.New()

	tenantStore := newCRUDStore()

	f := &boardFixture{
		router: NewRouter(discardLogger(),
			HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
			AuthDeps{Service: &membershipService{issuer: issuer}, Verifier: issuer, Store: tenantStore},
			RealtimeDeps{}),
		store:   tenantStore,
		issuer:  issuer,
		tenantA: tenantA,
		tenantB: tenantB,
		alice:   tenantStore.seed(tenantA, "alpha"),
		bob:     tenantStore.seed(tenantB, "beta"),
	}

	token, _, err := issuer.Issue(auth.Principal{
		UserID: aliceID, TenantID: tenantA, Role: "owner", SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing alice's token: %v", err)
	}

	f.token = token

	return f
}

// boardCall is one endpoint, described twice over: once against the caller's own
// objects and once against the other tenant's.
type boardCall struct {
	name string

	method string

	// path is a template with %s where an object id goes, and ids names which
	// ids fill it, in order.
	path string
	ids  func(o *tenantObjects) []any

	// body is built from the same tenant's objects, so a move against bob's
	// world names bob's column and bob's anchor card.
	body func(o *tenantObjects) any

	// selfStatus is what the control must answer.
	selfStatus int
}

// boardCalls is every route mountBoardRoutes mounts that takes an object id.
//
// POST /projects and GET /projects are excluded on purpose: they carry no
// object id, so there is nothing to attack. They are the control in
// TestBoardSurfaceControlWorks instead.
func boardCalls() []boardCall {
	return []boardCall{
		{
			name: "GET /projects/:id", method: http.MethodGet, path: "/api/v1/projects/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.project.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "PATCH /projects/:id", method: http.MethodPatch, path: "/api/v1/projects/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.project.ID} },
			body:       func(*tenantObjects) any { return map[string]string{"name": "renamed by alice"} },
			selfStatus: http.StatusOK,
		},
		{
			name: "POST /projects/:id/archive", method: http.MethodPost, path: "/api/v1/projects/%s/archive",
			ids:        func(o *tenantObjects) []any { return []any{o.project.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "POST /projects/:id/boards", method: http.MethodPost, path: "/api/v1/projects/%s/boards",
			ids:        func(o *tenantObjects) []any { return []any{o.project.ID} },
			body:       func(*tenantObjects) any { return map[string]string{"name": "board by alice"} },
			selfStatus: http.StatusCreated,
		},
		{
			name: "GET /projects/:id/boards", method: http.MethodGet, path: "/api/v1/projects/%s/boards",
			ids:        func(o *tenantObjects) []any { return []any{o.project.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "GET /boards/:id", method: http.MethodGet, path: "/api/v1/boards/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.board.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "PATCH /boards/:id", method: http.MethodPatch, path: "/api/v1/boards/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.board.ID} },
			body:       func(*tenantObjects) any { return map[string]string{"name": "renamed by alice"} },
			selfStatus: http.StatusOK,
		},
		{
			name: "DELETE /boards/:id", method: http.MethodDelete, path: "/api/v1/boards/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.board.ID} },
			selfStatus: http.StatusNoContent,
		},
		{
			name: "POST /boards/:id/columns", method: http.MethodPost, path: "/api/v1/boards/%s/columns",
			ids:        func(o *tenantObjects) []any { return []any{o.board.ID} },
			body:       func(*tenantObjects) any { return map[string]string{"name": "column by alice"} },
			selfStatus: http.StatusCreated,
		},
		{
			name: "GET /boards/:id/columns", method: http.MethodGet, path: "/api/v1/boards/%s/columns",
			ids:        func(o *tenantObjects) []any { return []any{o.board.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "GET /boards/:id/cards", method: http.MethodGet, path: "/api/v1/boards/%s/cards",
			ids:        func(o *tenantObjects) []any { return []any{o.board.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "PATCH /columns/:id", method: http.MethodPatch, path: "/api/v1/columns/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.column.ID} },
			body:       func(*tenantObjects) any { return map[string]string{"name": "renamed by alice"} },
			selfStatus: http.StatusOK,
		},
		{
			name: "POST /columns/:id/move", method: http.MethodPost, path: "/api/v1/columns/%s/move",
			ids:        func(o *tenantObjects) []any { return []any{o.column.ID} },
			body:       func(*tenantObjects) any { return map[string]any{"after_column_id": nil} },
			selfStatus: http.StatusOK,
		},
		{
			name: "DELETE /columns/:id", method: http.MethodDelete, path: "/api/v1/columns/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.column.ID} },
			selfStatus: http.StatusNoContent,
		},
		{
			name: "POST /columns/:id/cards", method: http.MethodPost, path: "/api/v1/columns/%s/cards",
			ids:        func(o *tenantObjects) []any { return []any{o.column.ID} },
			body:       func(*tenantObjects) any { return map[string]string{"title": "card by alice"} },
			selfStatus: http.StatusCreated,
		},
		{
			name: "GET /columns/:id/cards", method: http.MethodGet, path: "/api/v1/columns/%s/cards",
			ids:        func(o *tenantObjects) []any { return []any{o.column.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "GET /cards/:id", method: http.MethodGet, path: "/api/v1/cards/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.card.ID} },
			selfStatus: http.StatusOK,
		},
		{
			name: "PATCH /cards/:id", method: http.MethodPatch, path: "/api/v1/cards/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.card.ID} },
			body:       func(*tenantObjects) any { return map[string]string{"title": "retitled by alice"} },
			selfStatus: http.StatusOK,
		},
		{
			// The move is the interesting one: it names two objects, so the
			// cross-tenant version is bob's card into bob's column — the whole
			// operation transplanted, which is the strongest form of the attack.
			name: "POST /cards/:id/move", method: http.MethodPost, path: "/api/v1/cards/%s/move",
			ids: func(o *tenantObjects) []any { return []any{o.card.ID} },
			body: func(o *tenantObjects) any {
				return map[string]any{"column_id": o.column.ID.String(), "after_card_id": nil}
			},
			selfStatus: http.StatusOK,
		},
		{
			name: "DELETE /cards/:id", method: http.MethodDelete, path: "/api/v1/cards/%s",
			ids:        func(o *tenantObjects) []any { return []any{o.card.ID} },
			selfStatus: http.StatusNoContent,
		},
	}
}

// TestBoardSurfaceControlWorks is the control for everything below: alice's own
// requests succeed and return alice's data.
//
// Without it, every cross-tenant assertion in this file would also hold for a
// router that refused every request.
func TestBoardSurfaceControlWorks(t *testing.T) {
	t.Parallel()

	for _, call := range boardCalls() {
		t.Run(call.name, func(t *testing.T) {
			t.Parallel()

			f := newBoardFixture(t)

			rec := f.do(t, call.method, call.pathFor(f.alice), call.bodyFor(f.alice), nil)

			t.Logf("alice, own object -> %d %s", rec.Code, truncate(rec.Body.String()))

			if rec.Code != call.selfStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, call.selfStatus, rec.Body.String())
			}

			// Every 2xx with a body must carry alice's marker. A 204 has none,
			// and its status is the whole assertion.
			if rec.Code != http.StatusNoContent && !bytes.Contains(rec.Body.Bytes(), []byte("alpha")) {
				t.Errorf("a successful response carried none of alice's data: %s", rec.Body.String())
			}
		})
	}
}

// TestAnAuthenticatedUserCannotReachAnotherTenantsBoardObjects is the headline.
func TestAnAuthenticatedUserCannotReachAnotherTenantsBoardObjects(t *testing.T) {
	t.Parallel()

	for _, call := range boardCalls() {
		t.Run(call.name, func(t *testing.T) {
			t.Parallel()

			f := newBoardFixture(t)

			t.Logf("alice is a member of %s only; the ids in this request belong to %s", f.tenantA, f.tenantB)

			before := len(f.store.openedTenants())

			rec := f.do(t, call.method, call.pathFor(f.bob), call.bodyFor(f.bob), nil)

			t.Logf("alice, bob's object -> %d %s", rec.Code, truncate(rec.Body.String()))

			f.assertNoBobData(t, rec.Body.Bytes())
			f.assertOnlyAliceOpened(t, before)

			// A 2xx would mean the operation was carried out on bob's object.
			// 404 is the expected answer; a list endpoint answers 200 with an
			// empty collection, which assertNoBobData has already checked.
			if rec.Code >= 200 && rec.Code < 300 && call.method != http.MethodGet {
				t.Errorf("a write against another tenant's object answered %d, want a refusal", rec.Code)
			}
		})
	}
}

// TestMovingOwnCardIntoAnotherTenantsColumnIsRefused is the mixed-ownership
// case: every id in the request is real, and one of them is not the caller's.
//
// This is the shape a nested-resource API gets wrong. Nothing here validates
// that the column "belongs to" alice — it does not have to, because inside her
// tenant context bob's column does not exist.
func TestMovingOwnCardIntoAnotherTenantsColumnIsRefused(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	before := len(f.store.openedTenants())

	rec := f.do(t, http.MethodPost, "/api/v1/cards/"+f.alice.card.ID.String()+"/move",
		map[string]any{"column_id": f.bob.column.ID.String()}, nil)

	t.Logf("alice moving her own card into bob's column -> %d %s", rec.Code, truncate(rec.Body.String()))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — bob's column must not exist to alice", rec.Code)
	}

	f.assertNoBobData(t, rec.Body.Bytes())
	f.assertOnlyAliceOpened(t, before)

	// And the anchor, which is the other id a move carries.
	before = len(f.store.openedTenants())

	rec = f.do(t, http.MethodPost, "/api/v1/cards/"+f.alice.card.ID.String()+"/move",
		map[string]any{"column_id": f.alice.column.ID.String(), "after_card_id": f.bob.card.ID.String()}, nil)

	t.Logf("alice moving her card after bob's card -> %d %s", rec.Code, truncate(rec.Body.String()))

	if rec.Code < 400 {
		t.Errorf("status = %d, want a refusal: an anchor from another tenant must not be honoured", rec.Code)
	}

	f.assertNoBobData(t, rec.Body.Bytes())
	f.assertOnlyAliceOpened(t, before)
}

// TestTenantCannotBeOverriddenOnTheBoardSurface repeats auth_bola_test.go's
// injection channels against a board endpoint.
//
// It is not redundant with that file: the middleware is shared, but these
// handlers are new, and "no handler merges request data into the principal" is a
// claim about handlers.
func TestTenantCannotBeOverriddenOnTheBoardSurface(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	for _, attack := range []struct {
		name   string
		path   string
		header string
	}{
		{name: "X-Tenant-ID header", path: "/api/v1/projects", header: "X-Tenant-ID"},
		{name: "X-Organization-ID header", path: "/api/v1/projects", header: "X-Organization-ID"},
		{name: "X-Org-Id header on a card read", path: "/api/v1/cards/" + f.bob.card.ID.String(), header: "X-Org-Id"},
		{name: "org query parameter", path: "/api/v1/projects?org=" + f.tenantB.String()},
		{name: "tenant_id query parameter", path: "/api/v1/projects?tenant_id=" + f.tenantB.String()},
		{name: "an organization in the path", path: "/api/v1/organizations/" + f.tenantB.String() + "/cards"},
	} {
		t.Run(attack.name, func(t *testing.T) {
			headers := map[string]string{}
			if attack.header != "" {
				headers[attack.header] = f.tenantB.String()
			}

			before := len(f.store.openedTenants())

			rec := f.do(t, http.MethodGet, attack.path, nil, headers)

			t.Logf("%s -> %d %s", attack.name, rec.Code, truncate(rec.Body.String()))

			f.assertNoBobData(t, rec.Body.Bytes())
			f.assertOnlyAliceOpened(t, before)
		})
	}
}

// TestBoardBOLAAssertionsHaveTeeth builds the vulnerable version and requires
// the attack to work against it.
//
// The vulnerability is not a straw man: "resolve the object by id, then scope
// the query" is how this surface is usually written, and it is the exact bug the
// real handlers avoid by never resolving anything outside a tenant transaction.
// Here it is expressed as a middleware that lets a header choose the tenant,
// which reaches the same end through the shorter road.
//
// If this test ever stops failing to leak, the assertions above are measuring
// nothing.
func TestBoardBOLAAssertionsHaveTeeth(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	gin.SetMode(gin.TestMode)

	vulnerable := gin.New()
	vulnerable.GET("/api/v1/cards/:card_id",
		requireAuth(discardLogger(), f.issuer),
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
		getCardHandler(discardLogger(), f.store))

	before := len(f.store.openedTenants())

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/cards/"+f.bob.card.ID.String(), nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("X-Organization-ID", f.tenantB.String())
	vulnerable.ServeHTTP(rec, req)

	t.Logf("the same request against a handler behind a tenant-overriding middleware -> %d %s",
		rec.Code, truncate(rec.Body.String()))

	if !bytes.Contains(rec.Body.Bytes(), []byte(f.bob.card.Title)) {
		t.Fatal("the deliberately vulnerable router did not leak; the assertions above cannot be trusted to detect a leak")
	}

	opened := f.store.openedTenants()[before:]

	t.Logf("tenant contexts opened by the vulnerable router: %v", opened)

	if !slices.Contains(opened, f.tenantB) {
		t.Fatal("the vulnerable router opened no foreign tenant context; assertOnlyAliceOpened cannot detect one either")
	}

	t.Log("confirmed: bypassing the tenant-from-claim rule returns another organization's card, and both assertions catch it")
}

func (f *boardFixture) assertNoBobData(t *testing.T, body []byte) {
	t.Helper()

	for _, secret := range []struct{ what, value string }{
		{"the marker on every one of bob's rows", "beta"},
		{"bob's project id", f.bob.project.ID.String()},
		{"bob's board id", f.bob.board.ID.String()},
		{"bob's column id", f.bob.column.ID.String()},
		{"bob's card id", f.bob.card.ID.String()},
		{"bob's tenant id", f.tenantB.String()},
	} {
		if bytes.Contains(body, []byte(secret.value)) {
			t.Errorf("BOLA: the response contains %s (%s)\n%s", secret.what, secret.value, body)
		}
	}
}

func (f *boardFixture) assertOnlyAliceOpened(t *testing.T, from int) {
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

func (c boardCall) pathFor(o *tenantObjects) string {
	return fmt.Sprintf(c.path, c.ids(o)...)
}

func (c boardCall) bodyFor(o *tenantObjects) any {
	if c.body == nil {
		return nil
	}

	return c.body(o)
}

func (f *boardFixture) do(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	payload := bytes.NewReader(nil)

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encoding the body: %v", err)
		}

		payload = bytes.NewReader(encoded)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, path, payload)
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	return rec
}
