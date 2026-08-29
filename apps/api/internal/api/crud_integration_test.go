//go:build integration

package api_test

// The board surface end to end, against a real Postgres with the real policies.
//
// crud_bola_test.go asks "was a tenant context ever opened for the other
// organization", which is a question about the call and is best answered by a
// recording fake. This file asks the question a customer would ask — "whose rows
// came back, and is my board still the way I left it" — and only a database with
// row-level security actually running can answer that.
//
// Four things are proved here:
//
//  1. the whole hierarchy works: project -> board -> column -> card -> move,
//     with the order verified after each move;
//  2. an authenticated member of one organization cannot read, update, move or
//     delete any object of another, by id, on every endpoint — and the other
//     organization's board is byte-for-byte unchanged afterwards;
//  3. what actually happens when two clients move cards concurrently, measured
//     over repeated runs rather than asserted from a reading of the SQL;
//  4. that ranks are rebalanced before their precision becomes a problem, by
//     driving the nesting past the threshold.
//
// Positions are read through the owner pool in a few places. That is deliberate
// and is observation only: no code under test uses it, and the assertions it
// supports are about a column the API never publishes.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// hierarchy is one tenant's board, built through the real endpoints.
type hierarchy struct {
	owner    account
	project  uuid.UUID
	board    uuid.UUID
	todo     uuid.UUID
	doing    uuid.UUID
	cards    map[string]uuid.UUID
	cardList []uuid.UUID
}

// build creates a project, a board, two columns and the named cards, in order.
func build(t *testing.T, s *server, acct account, label string, titles ...string) hierarchy {
	t.Helper()

	h := hierarchy{owner: acct, cards: map[string]uuid.UUID{}}

	h.project = created(t, s, acct.accessToken, http.MethodPost, "/api/v1/projects", "project",
		map[string]string{"name": label + " project", "description": label + " description"})

	h.board = created(t, s, acct.accessToken, http.MethodPost,
		"/api/v1/projects/"+h.project.String()+"/boards", "board",
		map[string]string{"name": label + " board"})

	h.todo = created(t, s, acct.accessToken, http.MethodPost,
		"/api/v1/boards/"+h.board.String()+"/columns", "column",
		map[string]string{"name": label + " todo"})

	h.doing = created(t, s, acct.accessToken, http.MethodPost,
		"/api/v1/boards/"+h.board.String()+"/columns", "column",
		map[string]string{"name": label + " doing"})

	for _, title := range titles {
		id := created(t, s, acct.accessToken, http.MethodPost,
			"/api/v1/columns/"+h.todo.String()+"/cards", "card",
			map[string]string{"title": title})

		h.cards[title] = id
		h.cardList = append(h.cardList, id)
	}

	return h
}

// created posts and returns the id of the object in the named envelope.
func created(t *testing.T, s *server, token, method, path, envelope string, body any) uuid.UUID {
	t.Helper()

	resp := s.do(t, method, path, token, body)
	if resp.status != http.StatusCreated {
		t.Fatalf("%s %s: status %d, body %s", method, path, resp.status, resp.raw)
	}

	object, ok := resp.body[envelope].(map[string]any)
	if !ok {
		t.Fatalf("%s %s: no %q in the response: %s", method, path, envelope, resp.raw)
	}

	return uuid.MustParse(stringField(t, object, "id"))
}

// titlesIn reads a column's cards in the order the API returns them.
func titlesIn(t *testing.T, s *server, token string, columnID uuid.UUID) []string {
	t.Helper()

	resp := s.do(t, http.MethodGet, "/api/v1/columns/"+columnID.String()+"/cards", token, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("listing cards: status %d, body %s", resp.status, resp.raw)
	}

	return titlesFrom(t, resp)
}

func titlesFrom(t *testing.T, resp response) []string {
	t.Helper()

	raw, ok := resp.body["cards"].([]any)
	if !ok {
		t.Fatalf("no cards array in the response: %s", resp.raw)
	}

	titles := make([]string, 0, len(raw))

	for _, entry := range raw {
		card, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("a card is not an object: %s", resp.raw)
		}

		titles = append(titles, stringField(t, card, "title"))
	}

	return titles
}

// rankReader reads the ranks the API never publishes, for the assertions that
// are about the ranks themselves.
//
// It opens one pool for the whole test and returns a closure, rather than being
// a plain helper that opens a pool per call: the rebalance test calls it a
// hundred times, and a pool per call exhausts the container's connection slots
// long before the assertion it is there to make.
//
// The pool is the owner's and is observation only — nothing under test uses it.
// It has to be the owner's, in fact, precisely because the app role is the one
// the policies apply to and this is a question about rows across the test's own
// tenant that the API declines to answer.
func rankReader(t *testing.T) func(uuid.UUID) []string {
	t.Helper()

	owner := testDB.OwnerPool(t, 2)

	return func(columnID uuid.UUID) []string {
		t.Helper()

		// The output column is aliased and the ORDER BY is qualified on purpose.
		// `SELECT position::text ... ORDER BY position` sorts by the *output*
		// column, because SQL resolves a bare name in ORDER BY against the
		// select list first — which here is text, so 10 sorts before 2 and the
		// evidence this helper exists to produce reads as nonsense.
		rows, err := owner.Query(context.Background(),
			`SELECT position::text AS rank
			   FROM cards
			  WHERE column_id = $1
			  ORDER BY cards.position, cards.id`, columnID)
		if err != nil {
			t.Fatalf("reading positions: %v", err)
		}

		defer rows.Close()

		var positions []string

		for rows.Next() {
			var position string

			if err := rows.Scan(&position); err != nil {
				t.Fatalf("scanning a position: %v", err)
			}

			positions = append(positions, position)
		}

		if err := rows.Err(); err != nil {
			t.Fatalf("reading positions: %v", err)
		}

		return positions
	}
}

func moveCard(t *testing.T, s *server, token string, cardID, columnID uuid.UUID, after *uuid.UUID) response {
	t.Helper()

	body := map[string]any{"column_id": columnID.String()}
	if after != nil {
		body["after_card_id"] = after.String()
	}

	return s.do(t, http.MethodPost, "/api/v1/cards/"+cardID.String()+"/move", token, body)
}

// TestTheWholeHierarchyEndToEnd is the acceptance criterion: create project ->
// board -> column -> card -> move card -> verify order, through HTTP, against a
// real database.
func TestTheWholeHierarchyEndToEnd(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")
	ranks := rankReader(t)

	h := build(t, s, alice, "alpha", "one", "two", "three", "four")

	t.Logf("project=%s board=%s todo=%s doing=%s", h.project, h.board, h.todo, h.doing)

	assertOrder(t, s, alice.accessToken, h.todo, []string{"one", "two", "three", "four"},
		"cards are appended in creation order")

	// To the front.
	resp := moveCard(t, s, alice.accessToken, h.cards["four"], h.todo, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("moving four to the front: status %d, body %s", resp.status, resp.raw)
	}

	assertOrder(t, s, alice.accessToken, h.todo, []string{"four", "one", "two", "three"},
		"a null after_card_id means first")

	// Into a gap.
	four := h.cards["four"]
	resp = moveCard(t, s, alice.accessToken, h.cards["two"], h.todo, &four)

	if resp.status != http.StatusOK {
		t.Fatalf("moving two after four: status %d, body %s", resp.status, resp.raw)
	}

	assertOrder(t, s, alice.accessToken, h.todo, []string{"four", "two", "one", "three"},
		"a move into a gap lands between its neighbours")

	t.Logf("positions after two moves: %v", ranks(h.todo))

	// To the end.
	three := h.cards["three"]
	resp = moveCard(t, s, alice.accessToken, h.cards["four"], h.todo, &three)

	if resp.status != http.StatusOK {
		t.Fatalf("moving four to the end: status %d, body %s", resp.status, resp.raw)
	}

	assertOrder(t, s, alice.accessToken, h.todo, []string{"two", "one", "three", "four"},
		"an anchor with nothing after it means last")

	// Across columns.
	resp = moveCard(t, s, alice.accessToken, h.cards["one"], h.doing, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("moving one into doing: status %d, body %s", resp.status, resp.raw)
	}

	assertOrder(t, s, alice.accessToken, h.todo, []string{"two", "three", "four"},
		"the card leaves the column it came from")
	assertOrder(t, s, alice.accessToken, h.doing, []string{"one"},
		"and arrives in the one it was sent to")

	// The board-wide read the board view uses.
	board := s.do(t, http.MethodGet, "/api/v1/boards/"+h.board.String()+"/cards", alice.accessToken, nil)
	if board.status != http.StatusOK {
		t.Fatalf("listing the board's cards: status %d, body %s", board.status, board.raw)
	}

	t.Logf("GET /boards/:id/cards -> %d, %d cards", board.status, len(titlesFrom(t, board)))

	if got := len(titlesFrom(t, board)); got != 4 {
		t.Errorf("the board has %d cards, want 4", got)
	}

	// Rename, archive and delete, which are the rest of the surface.
	rename := s.do(t, http.MethodPatch, "/api/v1/cards/"+h.cards["two"].String(), alice.accessToken,
		map[string]string{"title": "two, renamed"})
	if rename.status != http.StatusOK {
		t.Fatalf("renaming a card: status %d, body %s", rename.status, rename.raw)
	}

	// A PATCH that mentions only the title must not blank the description.
	described := s.do(t, http.MethodPatch, "/api/v1/projects/"+h.project.String(), alice.accessToken,
		map[string]string{"name": "alpha project, renamed"})
	if described.status != http.StatusOK {
		t.Fatalf("renaming the project: status %d, body %s", described.status, described.raw)
	}

	project, ok := described.body["project"].(map[string]any)
	if !ok || project["description"] != "alpha description" {
		t.Errorf("renaming the project changed its description: %s", described.raw)
	}

	archive := s.do(t, http.MethodPost, "/api/v1/projects/"+h.project.String()+"/archive", alice.accessToken, nil)
	if archive.status != http.StatusOK {
		t.Fatalf("archiving: status %d, body %s", archive.status, archive.raw)
	}

	again := s.do(t, http.MethodPost, "/api/v1/projects/"+h.project.String()+"/archive", alice.accessToken, nil)

	t.Logf("archiving twice -> %d then %d", archive.status, again.status)

	if again.status != http.StatusOK {
		t.Errorf("archiving an archived project answered %d; the operation is meant to be idempotent", again.status)
	}

	list := s.do(t, http.MethodGet, "/api/v1/projects", alice.accessToken, nil)
	if projects, ok := list.body["projects"].([]any); !ok || len(projects) != 0 {
		t.Errorf("an archived project is still listed: %s", list.raw)
	}

	deleted := s.do(t, http.MethodDelete, "/api/v1/boards/"+h.board.String(), alice.accessToken, nil)
	if deleted.status != http.StatusNoContent {
		t.Fatalf("deleting the board: status %d, body %s", deleted.status, deleted.raw)
	}

	gone := s.do(t, http.MethodGet, "/api/v1/cards/"+h.cards["three"].String(), alice.accessToken, nil)

	t.Logf("a card whose board was deleted -> %d %s", gone.status, gone.raw)

	if gone.status != http.StatusNotFound {
		t.Errorf("deleting a board left its cards reachable: %d %s", gone.status, gone.raw)
	}
}

// TestMoveRefusesAStaleOrForeignAnchor covers the two 409s.
func TestMoveRefusesAStaleOrForeignAnchor(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	h := build(t, s, alice, "alpha", "one", "two")

	// An anchor that is in another column of the same board.
	two := h.cards["two"]

	moved := moveCard(t, s, alice.accessToken, h.cards["one"], h.doing, &two)

	t.Logf("anchor in a different column -> %d %s", moved.status, moved.raw)

	if moved.status != http.StatusConflict {
		t.Errorf("status = %d, want 409", moved.status)
	}

	// An anchor that does not exist at all.
	missing := uuid.New()

	moved = moveCard(t, s, alice.accessToken, h.cards["one"], h.todo, &missing)

	t.Logf("anchor that does not exist -> %d %s", moved.status, moved.raw)

	if moved.status != http.StatusConflict {
		t.Errorf("status = %d, want 409", moved.status)
	}

	// The order is untouched by either refusal.
	assertOrder(t, s, alice.accessToken, h.todo, []string{"one", "two"}, "a refused move changes nothing")

	// A column on another board of the same tenant.
	other := build(t, s, alice, "beta")

	moved = moveCard(t, s, alice.accessToken, h.cards["one"], other.todo, nil)

	t.Logf("target column on another board -> %d %s", moved.status, moved.raw)

	if moved.status != http.StatusConflict {
		t.Errorf("status = %d, want 409 — cards do not change board", moved.status)
	}
}

// TestNoTenantCanReachAnotherTenantsBoardOverHTTP is the cross-tenant matrix
// against a real database: every endpoint, by id, in both directions.
//
// The second half is the part a status-code-only test would miss: after every
// attack, the victim re-reads their own board and it must be exactly what it was
// before. A handler that wrote and then answered 404 would pass on status alone.
func TestNoTenantCanReachAnotherTenantsBoardOverHTTP(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	alpha := build(t, s, alice, "alpha", "alpha-one", "alpha-two")
	beta := build(t, s, bob, "beta", "beta-one", "beta-two")

	t.Logf("alice tenant=%s board=%s", alice.tenantID, alpha.board)
	t.Logf("bob   tenant=%s board=%s", bob.tenantID, beta.board)

	for _, direction := range []struct {
		name             string
		attacker, victim hierarchy
	}{
		{name: "alice against bob", attacker: alpha, victim: beta},
		{name: "bob against alice", attacker: beta, victim: alpha},
	} {
		t.Run(direction.name, func(t *testing.T) {
			token := direction.attacker.owner.accessToken
			victimToken := direction.victim.owner.accessToken
			v := direction.victim

			before := snapshot(t, s, victimToken, v)

			for _, attack := range crossTenantCalls(v) {
				t.Run(attack.name, func(t *testing.T) {
					resp := s.do(t, attack.method, attack.path, token, attack.body)

					t.Logf("%s -> %d %s", attack.name, resp.status, resp.raw)

					if resp.status >= 200 && resp.status < 300 && attack.method != http.MethodGet {
						t.Errorf("a write against another tenant's object answered %d", resp.status)
					}

					for _, marker := range v.markers() {
						if strings.Contains(resp.raw, marker) {
							t.Errorf("the response carries the other tenant's data (%s): %s", marker, resp.raw)
						}
					}
				})
			}

			after := snapshot(t, s, victimToken, v)

			t.Logf("victim's board before: %v", before)
			t.Logf("victim's board after:  %v", after)

			if fmt.Sprint(before) != fmt.Sprint(after) {
				t.Errorf("the victim's board changed under attack:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
}

// markers are the ids that appear only in this tenant's rows. Ids rather than
// names, because an id is what an attacker would have leaked and what a leaking
// response would echo.
func (h hierarchy) markers() []string {
	markers := []string{h.project.String(), h.board.String(), h.todo.String(), h.doing.String()}
	for _, id := range h.cardList {
		markers = append(markers, id.String())
	}

	return markers
}

type crossTenantCall struct {
	name   string
	method string
	path   string
	body   any
}

// crossTenantCalls is every endpoint that takes an object id, aimed at the
// victim's objects.
func crossTenantCalls(v hierarchy) []crossTenantCall {
	return []crossTenantCall{
		{"GET /projects/:id", http.MethodGet, "/api/v1/projects/" + v.project.String(), nil},
		{"PATCH /projects/:id", http.MethodPatch, "/api/v1/projects/" + v.project.String(), map[string]string{"name": "seized"}},
		{"POST /projects/:id/archive", http.MethodPost, "/api/v1/projects/" + v.project.String() + "/archive", nil},
		{"DELETE /projects/:id/archive", http.MethodDelete, "/api/v1/projects/" + v.project.String() + "/archive", nil},
		{"POST /projects/:id/boards", http.MethodPost, "/api/v1/projects/" + v.project.String() + "/boards", map[string]string{"name": "seized"}},
		{"GET /projects/:id/boards", http.MethodGet, "/api/v1/projects/" + v.project.String() + "/boards", nil},
		{"GET /boards/:id", http.MethodGet, "/api/v1/boards/" + v.board.String(), nil},
		{"PATCH /boards/:id", http.MethodPatch, "/api/v1/boards/" + v.board.String(), map[string]string{"name": "seized"}},
		{"POST /boards/:id/columns", http.MethodPost, "/api/v1/boards/" + v.board.String() + "/columns", map[string]string{"name": "seized"}},
		{"GET /boards/:id/columns", http.MethodGet, "/api/v1/boards/" + v.board.String() + "/columns", nil},
		{"GET /boards/:id/cards", http.MethodGet, "/api/v1/boards/" + v.board.String() + "/cards", nil},
		{"PATCH /columns/:id", http.MethodPatch, "/api/v1/columns/" + v.todo.String(), map[string]string{"name": "seized"}},
		{"POST /columns/:id/move", http.MethodPost, "/api/v1/columns/" + v.todo.String() + "/move", map[string]any{"after_column_id": v.doing.String()}},
		{"POST /columns/:id/cards", http.MethodPost, "/api/v1/columns/" + v.todo.String() + "/cards", map[string]string{"title": "seized"}},
		{"GET /columns/:id/cards", http.MethodGet, "/api/v1/columns/" + v.todo.String() + "/cards", nil},
		{"GET /cards/:id", http.MethodGet, "/api/v1/cards/" + v.cardList[0].String(), nil},
		{"PATCH /cards/:id", http.MethodPatch, "/api/v1/cards/" + v.cardList[0].String(), map[string]string{"title": "seized"}},
		{
			"POST /cards/:id/move", http.MethodPost, "/api/v1/cards/" + v.cardList[0].String() + "/move",
			map[string]any{"column_id": v.doing.String()},
		},
		{"DELETE /cards/:id", http.MethodDelete, "/api/v1/cards/" + v.cardList[0].String(), nil},
		{"DELETE /columns/:id", http.MethodDelete, "/api/v1/columns/" + v.todo.String(), nil},
		{"DELETE /boards/:id", http.MethodDelete, "/api/v1/boards/" + v.board.String(), nil},
	}
}

// snapshot is everything about the victim's board that an attack could change.
func snapshot(t *testing.T, s *server, token string, h hierarchy) []string {
	t.Helper()

	state := []string{}

	projects := s.do(t, http.MethodGet, "/api/v1/projects", token, nil)
	state = append(state, "projects="+projects.raw)

	boards := s.do(t, http.MethodGet, "/api/v1/projects/"+h.project.String()+"/boards", token, nil)
	state = append(state, "boards="+boards.raw)

	columns := s.do(t, http.MethodGet, "/api/v1/boards/"+h.board.String()+"/columns", token, nil)
	state = append(state, "columns="+columns.raw)

	cards := s.do(t, http.MethodGet, "/api/v1/boards/"+h.board.String()+"/cards", token, nil)
	state = append(state, "cards="+cards.raw)

	return state
}

// TestTwoClientsMovingTheSameCardConcurrently demonstrates the behaviour the
// ordering strategy was chosen for.
//
// It does not assert a winner — there is no winner to assert, and a test that
// picked one would be asserting the scheduler. It asserts the invariants that
// have to hold whoever wins, and *reports* the distribution over repeated runs
// so the behaviour is observed rather than described.
func TestTwoClientsMovingTheSameCardConcurrently(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	ranks := rankReader(t)

	const rounds = 12

	outcomes := map[string]int{}

	for round := range rounds {
		h := build(t, s, alice, fmt.Sprintf("round%d", round), "a", "b", "c", "d")

		anchorA := h.cards["a"]
		anchorB := h.cards["b"]

		var wg sync.WaitGroup

		statuses := make([]int, 2)

		wg.Add(2)

		start := make(chan struct{})

		go func() {
			defer wg.Done()

			<-start

			statuses[0] = moveCard(t, s, alice.accessToken, h.cards["d"], h.todo, &anchorA).status
		}()

		go func() {
			defer wg.Done()

			<-start

			statuses[1] = moveCard(t, s, alice.accessToken, h.cards["d"], h.todo, &anchorB).status
		}()

		close(start)
		wg.Wait()

		order := titlesIn(t, s, alice.accessToken, h.todo)
		positions := ranks(h.todo)

		outcomes[fmt.Sprint(order)]++

		// Whoever won, these have to hold.
		if len(order) != 4 {
			t.Fatalf("round %d: %d cards after two concurrent moves, want 4: %v", round, len(order), order)
		}

		if duplicated(positions) {
			t.Fatalf("round %d: two cards share a rank: %v (order %v)", round, positions, order)
		}

		if statuses[0] != http.StatusOK || statuses[1] != http.StatusOK {
			t.Errorf("round %d: statuses %v; both moves name a live anchor and should both succeed",
				round, statuses)
		}

		// The only two orders either interleaving can produce.
		got := fmt.Sprint(order)
		if got != fmt.Sprint([]string{"a", "d", "b", "c"}) && got != fmt.Sprint([]string{"a", "b", "d", "c"}) {
			t.Errorf("round %d: order %v is neither of the two requested placements", round, order)
		}
	}

	t.Logf("over %d rounds of two clients moving the same card at once:", rounds)

	for order, count := range outcomes {
		t.Logf("  %s x%d", order, count)
	}

	if len(outcomes) == 0 {
		t.Fatal("no rounds ran")
	}
}

// TestTwoClientsMovingDifferentCardsIntoTheSameGap is the case that would
// produce two cards with the same rank if position allocation were not
// serialised on the column.
//
// Both clients compute a midpoint between the same pair of neighbours. Without
// the FOR UPDATE on the column they compute the *same* midpoint, and the column
// ends up with two cards whose order is then decided by the id tiebreaker
// forever. With it, the second one sees the first's card and subdivides again.
func TestTwoClientsMovingDifferentCardsIntoTheSameGap(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	ranks := rankReader(t)

	const rounds = 12

	outcomes := map[string]int{}

	for round := range rounds {
		h := build(t, s, alice, fmt.Sprintf("gap%d", round), "a", "b", "x", "y")

		anchor := h.cards["a"]

		var wg sync.WaitGroup

		wg.Add(2)

		start := make(chan struct{})

		for _, card := range []string{"x", "y"} {
			go func() {
				defer wg.Done()

				<-start

				moveCard(t, s, alice.accessToken, h.cards[card], h.todo, &anchor)
			}()
		}

		close(start)
		wg.Wait()

		order := titlesIn(t, s, alice.accessToken, h.todo)
		positions := ranks(h.todo)

		outcomes[fmt.Sprint(order)]++

		if duplicated(positions) {
			t.Fatalf("round %d: two cards share a rank: %v (order %v)", round, positions, order)
		}

		// Both land in the gap, in one order or the other; a and b keep their
		// relative order.
		got := fmt.Sprint(order)
		if got != fmt.Sprint([]string{"a", "x", "y", "b"}) && got != fmt.Sprint([]string{"a", "y", "x", "b"}) {
			t.Errorf("round %d: order %v; both cards should be between a and b", round, order)
		}
	}

	t.Logf("over %d rounds of two clients dropping different cards into the same gap:", rounds)

	for order, count := range outcomes {
		t.Logf("  %s x%d", order, count)
	}
}

// TestRanksAreRebalancedBeforePrecisionRuns is the cost of fractional ranking,
// driven until it actually happens.
//
// Each iteration appends a card and moves it into the same gap, which halves the
// gap and adds one decimal place. Past the threshold in query.sql the move
// renumbers the column, and the order is unchanged across that.
func TestRanksAreRebalancedBeforePrecisionRuns(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	ranks := rankReader(t)

	h := build(t, s, alice, "deep", "head", "tail")

	anchor := h.cards["head"]

	// The threshold in query.sql is a scale of 100, and each nested move adds
	// exactly one decimal place, so this crosses it with room to spare.
	const nested = 110

	rebalanced := false

	for i := range nested {
		title := fmt.Sprintf("nested-%03d", i)

		id := created(t, s, alice.accessToken, http.MethodPost,
			"/api/v1/columns/"+h.todo.String()+"/cards", "card",
			map[string]string{"title": title})

		resp := moveCard(t, s, alice.accessToken, id, h.todo, &anchor)
		if resp.status != http.StatusOK {
			t.Fatalf("nested move %d: status %d, body %s", i, resp.status, resp.raw)
		}

		positions := ranks(h.todo)
		if !rebalanced && allIntegral(positions) && i > 0 {
			rebalanced = true

			t.Logf("ranks were renumbered after %d nested moves; positions are now %v", i+1, positions)
		}

		if duplicated(positions) {
			t.Fatalf("nested move %d produced a duplicate rank: %v", i, positions)
		}
	}

	order := titlesIn(t, s, alice.accessToken, h.todo)

	t.Logf("final positions: %v", ranks(h.todo))

	if !rebalanced {
		t.Errorf("%d nested moves did not trigger a rebalance; the threshold or the scale arithmetic has changed", nested)
	}

	// head first, then the nested cards newest-first, then tail.
	if order[0] != "head" || order[len(order)-1] != "tail" {
		t.Errorf("the rebalance changed the order: %v", order)
	}

	if got := len(order); got != nested+2 {
		t.Errorf("%d cards in the column, want %d", got, nested+2)
	}

	for i := 1; i < len(order)-1; i++ {
		want := fmt.Sprintf("nested-%03d", nested-i)
		if order[i] != want {
			t.Fatalf("position %d is %q, want %q — each nested move should land directly after head: %v",
				i, order[i], want, order)
		}
	}
}

func assertOrder(t *testing.T, s *server, token string, columnID uuid.UUID, want []string, why string) {
	t.Helper()

	got := titlesIn(t, s, token, columnID)

	t.Logf("%s: %v", why, got)

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: order = %v, want %v", why, got, want)
	}
}

func duplicated(values []string) bool {
	seen := map[string]struct{}{}

	for _, value := range values {
		if _, ok := seen[value]; ok {
			return true
		}

		seen[value] = struct{}{}
	}

	return false
}

func allIntegral(positions []string) bool {
	for _, position := range positions {
		if strings.Contains(position, ".") {
			return false
		}
	}

	return len(positions) > 0
}

// Assignee and due date, against a real database — because the interesting
// properties here are all enforced by the schema rather than by Go.
//
// `00003_domain.sql` gives cards an assignee_id whose foreign key is
// deliberately against `memberships (tenant_id, user_id)` rather than `users`,
// with `ON DELETE SET NULL (assignee_id)`. Two consequences follow from that
// choice and neither is observable without a database: a card cannot be
// assigned to somebody outside its organization, and revoking a membership
// un-assigns their cards instead of failing.
func TestAssigneeAndDueDate(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	alpha := build(t, s, alice, "alpha", "alpha-one")

	card := alpha.cards["alpha-one"]
	cardPath := "/api/v1/cards/" + card.String()

	t.Run("a new card has neither, and says so explicitly", func(t *testing.T) {
		resp := s.do(t, http.MethodGet, cardPath, alice.accessToken, nil)

		t.Logf("fresh card -> %d %s", resp.status, resp.raw)

		// Present-and-null rather than omitted. A client replacing a card
		// wholesale from an event payload has to be able to tell "unassigned"
		// from "this body does not mention assignment".
		if !strings.Contains(resp.raw, `"assignee_id":null`) {
			t.Errorf("card body should carry an explicit null assignee_id: %s", resp.raw)
		}

		if !strings.Contains(resp.raw, `"due_at":null`) {
			t.Errorf("card body should carry an explicit null due_at: %s", resp.raw)
		}
	})

	t.Run("assign to self and set a due date", func(t *testing.T) {
		resp := s.do(t, http.MethodPatch, cardPath, alice.accessToken, map[string]any{
			"assignee_id": alice.userID.String(),
			"due_at":      "2026-12-31T17:00:00Z",
		})

		t.Logf("assign -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.raw)
		}

		if !strings.Contains(resp.raw, alice.userID.String()) {
			t.Errorf("response does not carry the assignee: %s", resp.raw)
		}
	})

	// The distinction the Optional type exists for. An absent key must leave
	// the assignee alone; a *pointer* would report both as nil and this edit
	// would silently unassign.
	t.Run("editing the title leaves the assignee alone", func(t *testing.T) {
		resp := s.do(t, http.MethodPatch, cardPath, alice.accessToken,
			map[string]any{"title": "renamed, still assigned"})

		t.Logf("title only -> %d %s", resp.status, resp.raw)

		if !strings.Contains(resp.raw, alice.userID.String()) {
			t.Errorf("a title-only patch dropped the assignee: %s", resp.raw)
		}
	})

	t.Run("explicit null unassigns", func(t *testing.T) {
		resp := s.do(t, http.MethodPatch, cardPath, alice.accessToken,
			map[string]any{"assignee_id": nil})

		t.Logf("unassign -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.raw)
		}

		if !strings.Contains(resp.raw, `"assignee_id":null`) {
			t.Errorf("explicit null did not unassign: %s", resp.raw)
		}

		// And it is a change, so it must not be refused as an empty patch.
		if resp.status == http.StatusBadRequest {
			t.Error("unassigning was treated as 'nothing was asked for'")
		}
	})

	t.Run("a malformed due date is refused", func(t *testing.T) {
		resp := s.do(t, http.MethodPatch, cardPath, alice.accessToken,
			map[string]any{"due_at": "31/12/2026"})

		t.Logf("bad due_at -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (%s)", resp.status, resp.raw)
		}
	})
}

// The property the composite foreign key exists for, and the one that would be
// a cross-tenant data leak if it failed: alice cannot assign her card to bob,
// who is a real user in a different organization.
//
// The answer must also be indistinguishable from assigning to a user id that
// names nobody at all. A different status or message for the two would let
// anyone with a card to edit probe for user ids across the tenant boundary, one
// guess at a time.
func TestACardCannotBeAssignedAcrossTheTenantBoundary(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	alpha := build(t, s, alice, "alpha", "alpha-one")
	cardPath := "/api/v1/cards/" + alpha.cards["alpha-one"].String()

	t.Logf("alice=%s (tenant %s)  bob=%s (tenant %s)",
		alice.userID, alice.tenantID, bob.userID, bob.tenantID)

	realUserElsewhere := s.do(t, http.MethodPatch, cardPath, alice.accessToken,
		map[string]any{"assignee_id": bob.userID.String()})

	fictionalUser := s.do(t, http.MethodPatch, cardPath, alice.accessToken,
		map[string]any{"assignee_id": uuid.NewString()})

	t.Logf("bob (real, other tenant) -> %d %s", realUserElsewhere.status, realUserElsewhere.raw)
	t.Logf("nobody at all            -> %d %s", fictionalUser.status, fictionalUser.raw)

	if realUserElsewhere.status == http.StatusOK {
		t.Fatal("a card was assigned to a member of another organization")
	}

	if realUserElsewhere.status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", realUserElsewhere.status)
	}

	// The assertion that matters: the two are the same answer.
	if realUserElsewhere.status != fictionalUser.status {
		t.Errorf("a real user in another tenant (%d) answers differently from a fictional one (%d): "+
			"this is a membership oracle across the tenant boundary",
			realUserElsewhere.status, fictionalUser.status)
	}

	if realUserElsewhere.raw != fictionalUser.raw {
		t.Errorf("the two answers differ in body:\n  real elsewhere: %s\n  fictional:      %s",
			realUserElsewhere.raw, fictionalUser.raw)
	}

	// And nothing about bob leaked into the refusal.
	if strings.Contains(realUserElsewhere.raw, bob.userID.String()) ||
		strings.Contains(realUserElsewhere.raw, bob.email) {
		t.Errorf("the refusal echoes the other tenant's user: %s", realUserElsewhere.raw)
	}
}

// The behaviour `ON DELETE SET NULL (assignee_id)` exists for, which nothing
// exercised until now.
//
// `00003_domain.sql` points the assignee foreign key at
// `memberships (tenant_id, user_id)` rather than at `users`, with a
// column-list SET NULL. The column list is the whole point: without it the
// action would try to null `tenant_id` too, which is NOT NULL, and revoking a
// membership would fail with a constraint violation instead of tidying up
// after itself. So somebody leaving an organization un-assigns their cards
// rather than making the membership undeletable.
//
// There is no endpoint that revokes a membership yet, so this goes through the
// owner pool -- the same route the registration cleanup uses. That is honest
// about what is being tested: the schema's behaviour, not an HTTP surface that
// does not exist.
func TestRevokingAMembershipUnassignsTheirCardsRatherThanFailing(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	// Bob joins alice's organization, so he is assignable there.
	added := s.do(t, http.MethodPost, "/api/v1/members", alice.accessToken,
		map[string]any{"email": bob.email, "role": "member"})

	t.Logf("add bob to alice's org -> %d %s", added.status, added.raw)

	if added.status != http.StatusCreated && added.status != http.StatusOK {
		t.Fatalf("could not add bob as a member: %d %s", added.status, added.raw)
	}

	alpha := build(t, s, alice, "alpha", "alpha-one")
	cardPath := "/api/v1/cards/" + alpha.cards["alpha-one"].String()

	assigned := s.do(t, http.MethodPatch, cardPath, alice.accessToken,
		map[string]any{"assignee_id": bob.userID.String()})

	t.Logf("assign to bob -> %d %s", assigned.status, assigned.raw)

	if assigned.status != http.StatusOK {
		t.Fatalf("assigning to a fellow member failed: %d %s", assigned.status, assigned.raw)
	}

	if !strings.Contains(assigned.raw, bob.userID.String()) {
		t.Fatalf("the card is not assigned to bob: %s", assigned.raw)
	}

	// Revoke it. This is the operation the FK's action governs.
	owner := testDB.OwnerPool(t, 2)

	tag, err := owner.Exec(context.Background(),
		`DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, alice.tenantID, bob.userID)
	if err != nil {
		t.Fatalf("revoking the membership failed, which is the bug this test exists to catch: %v", err)
	}

	if tag.RowsAffected() != 1 {
		t.Fatalf("expected to revoke exactly one membership, revoked %d", tag.RowsAffected())
	}

	after := s.do(t, http.MethodGet, cardPath, alice.accessToken, nil)

	t.Logf("card after revocation -> %d %s", after.status, after.raw)

	// The card survives -- it belongs to the organization, not to the assignee.
	if after.status != http.StatusOK {
		t.Fatalf("the card did not survive the revocation: %d %s", after.status, after.raw)
	}

	if !strings.Contains(after.raw, `"assignee_id":null`) {
		t.Errorf("the assignee should have been nulled by the revocation: %s", after.raw)
	}

	if strings.Contains(after.raw, bob.userID.String()) {
		t.Errorf("the card still names a user who is no longer a member: %s", after.raw)
	}
}

// Archiving used to be a one-way door: the project left the list, everything
// inside it stayed reachable if you already knew an id, and nothing in the API
// would tell you the project existed. A user who archived the wrong project had
// no way back through the product.
func TestArchiveAndUnarchiveRoundTrip(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	alpha := build(t, s, alice, "alpha", "alpha-one")

	projectPath := "/api/v1/projects/" + alpha.project.String()
	archivePath := projectPath + "/archive"

	listNames := func(t *testing.T, query string) string {
		t.Helper()

		resp := s.do(t, http.MethodGet, "/api/v1/projects"+query, alice.accessToken, nil)
		if resp.status != http.StatusOK {
			t.Fatalf("listing%s -> %d %s", query, resp.status, resp.raw)
		}

		return resp.raw
	}

	t.Run("a new project is in the default list and not the archived one", func(t *testing.T) {
		if !strings.Contains(listNames(t, ""), alpha.project.String()) {
			t.Error("a fresh project is missing from the default list")
		}

		if strings.Contains(listNames(t, "?archived=true"), alpha.project.String()) {
			t.Error("a fresh project appears in the archived list")
		}
	})

	t.Run("archiving moves it between the two lists", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, archivePath, alice.accessToken, nil)

		t.Logf("archive -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.status)
		}

		if strings.Contains(listNames(t, ""), alpha.project.String()) {
			t.Error("an archived project is still in the default list")
		}

		// The half that did not exist: it is now findable.
		if !strings.Contains(listNames(t, "?archived=true"), alpha.project.String()) {
			t.Error("an archived project is not in the archived list either, so it is unreachable")
		}
	})

	// The decision this PR makes explicit. Archiving means "stop showing me
	// this", not "seal it": somebody following a link to a card from a chat
	// message gets the card, not a wall.
	t.Run("a board inside an archived project is still readable by id", func(t *testing.T) {
		resp := s.do(t, http.MethodGet, "/api/v1/boards/"+alpha.board.String(), alice.accessToken, nil)

		t.Logf("board in archived project -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusOK {
			t.Errorf("status = %d, want 200: archiving is not a cascade and not a lock", resp.status)
		}
	})

	t.Run("unarchiving puts it back", func(t *testing.T) {
		resp := s.do(t, http.MethodDelete, archivePath, alice.accessToken, nil)

		t.Logf("unarchive -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (%s)", resp.status, resp.raw)
		}

		if !strings.Contains(resp.raw, `"archived_at":null`) {
			t.Errorf("the restored project still carries an archived_at: %s", resp.raw)
		}

		if !strings.Contains(listNames(t, ""), alpha.project.String()) {
			t.Error("an unarchived project is not back in the default list")
		}

		if strings.Contains(listNames(t, "?archived=true"), alpha.project.String()) {
			t.Error("an unarchived project is still in the archived list")
		}
	})

	// Same contract as archive, which is idempotent for the same reason: a
	// retried request is a success rather than a 404.
	t.Run("unarchiving an active project is idempotent", func(t *testing.T) {
		first := s.do(t, http.MethodDelete, archivePath, alice.accessToken, nil)
		second := s.do(t, http.MethodDelete, archivePath, alice.accessToken, nil)

		t.Logf("unarchive twice -> %d then %d", first.status, second.status)

		if first.status != http.StatusOK || second.status != http.StatusOK {
			t.Errorf("statuses = %d, %d; both should be 200", first.status, second.status)
		}
	})

	t.Run("archiving twice keeps the original timestamp", func(t *testing.T) {
		first := s.do(t, http.MethodPost, archivePath, alice.accessToken, nil)
		second := s.do(t, http.MethodPost, archivePath, alice.accessToken, nil)

		if first.status != http.StatusOK || second.status != http.StatusOK {
			t.Fatalf("statuses = %d, %d; both should be 200", first.status, second.status)
		}

		if first.raw != second.raw {
			t.Errorf("archiving twice changed the row:\n  first:  %s\n  second: %s", first.raw, second.raw)
		}
	})

	// An unparseable filter is an error, not a silent false. `?archived=yes` is
	// what people type, and answering it with the unfiltered list is a wrong
	// answer delivered with a 200.
	t.Run("an unparseable archived filter is refused", func(t *testing.T) {
		resp := s.do(t, http.MethodGet, "/api/v1/projects?archived=yes", alice.accessToken, nil)

		t.Logf("?archived=yes -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (%s)", resp.status, resp.raw)
		}
	})
}

// The archived list is tenant-scoped like every other list. Without this, a
// second collection reachable only through a query parameter is exactly the
// kind of surface that gets added without a policy behind it.
func TestTheArchivedListIsTenantScoped(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	alpha := build(t, s, alice, "alpha", "alpha-one")

	archived := s.do(t, http.MethodPost,
		"/api/v1/projects/"+alpha.project.String()+"/archive", alice.accessToken, nil)
	if archived.status != http.StatusOK {
		t.Fatalf("archiving failed: %d %s", archived.status, archived.raw)
	}

	resp := s.do(t, http.MethodGet, "/api/v1/projects?archived=true", bob.accessToken, nil)

	t.Logf("bob reads the archived list -> %d %s", resp.status, resp.raw)

	if strings.Contains(resp.raw, alpha.project.String()) ||
		strings.Contains(resp.raw, "alpha project") {
		t.Errorf("bob can see alice's archived project: %s", resp.raw)
	}
}
