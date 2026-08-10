//go:build integration

package api_test

// The move log, against a real Postgres.
//
// crud_bola_test.go's fake can be told to report NeedsRebalance; it cannot
// *reach* that branch, because the branch is decided by the numeric scale of a
// rank that a fake with no ranks does not have. Nor can it produce the
// cross-board 409, which needs two boards in one tenant. Both are here.
//
// The rest is here because #93 was reported from reading real logs, and a log
// assertion that only ever runs against a fake proves the handler called slog,
// not that a deployed instance emits anything useful.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// entriesSince decodes every log line written after a mark, so an assertion can
// be scoped to one request instead of to the whole test.
func entriesSince(t *testing.T, s *server, from int) []map[string]any {
	t.Helper()

	var entries []map[string]any

	for _, raw := range strings.Split(s.logs.String()[from:], "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}

		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}

		entries = append(entries, entry)
	}

	return entries
}

// entrySince returns the one line carrying an event, and fails otherwise.
func entrySince(t *testing.T, s *server, from int, event string) map[string]any {
	t.Helper()

	var found []map[string]any

	for _, entry := range entriesSince(t, s, from) {
		if entry["event"] == event {
			found = append(found, entry)
		}
	}

	if len(found) != 1 {
		t.Fatalf("found %d lines with event %q, want 1; log since the mark was:\n%s",
			len(found), event, s.logs.String()[from:])
	}

	return found[0]
}

func fieldsAre(t *testing.T, entry map[string]any, want map[string]any) {
	t.Helper()

	for field, expected := range want {
		got, present := entry[field]
		if !present {
			t.Errorf("field %q is missing from %v", field, entry)

			continue
		}

		if got != expected {
			t.Errorf("field %q = %#v, want %#v", field, got, expected)
		}
	}
}

func moveColumnTo(t *testing.T, s *server, token string, columnID uuid.UUID, after *uuid.UUID) response {
	t.Helper()

	body := map[string]any{}
	if after != nil {
		body["after_column_id"] = after.String()
	}

	return s.do(t, http.MethodPost, "/api/v1/columns/"+columnID.String()+"/move", token, body)
}

// TestTwoMovesAreDistinguishableInTheLog is the issue, restated as a test: the
// complaint was that eighteen moves produced eighteen identical lines.
func TestTwoMovesAreDistinguishableInTheLog(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	h := build(t, s, alice, "alpha", "one", "two", "three")

	first, second := h.cards["one"], h.cards["two"]
	anchor := h.cards["three"]

	mark := s.logs.Len()

	// Two moves of two different cards, into two different columns, one behind
	// an anchor and one to the front.
	if resp := moveCard(t, s, alice.accessToken, first, h.doing, nil); resp.status != http.StatusOK {
		t.Fatalf("first move: %d %s", resp.status, resp.raw)
	}

	if resp := moveCard(t, s, alice.accessToken, second, h.todo, &anchor); resp.status != http.StatusOK {
		t.Fatalf("second move: %d %s", resp.status, resp.raw)
	}

	var moves []map[string]any

	for _, entry := range entriesSince(t, s, mark) {
		if entry["event"] == "card.moved" {
			moves = append(moves, entry)
		}
	}

	if len(moves) != 2 {
		t.Fatalf("logged %d card.moved lines, want 2:\n%s", len(moves), s.logs.String()[mark:])
	}

	t.Logf("move 1: %v", moves[0])
	t.Logf("move 2: %v", moves[1])

	fieldsAre(t, moves[0], map[string]any{
		"level":          "INFO",
		"card_id":        first.String(),
		"board_id":       h.board.String(),
		"from_column_id": h.todo.String(),
		"to_column_id":   h.doing.String(),
		"tenant_id":      alice.tenantID.String(),
	})

	if moves[0]["after_card_id"] != nil {
		t.Errorf("a move to the front logged after_card_id = %#v, want null", moves[0]["after_card_id"])
	}

	fieldsAre(t, moves[1], map[string]any{
		"level":          "INFO",
		"card_id":        second.String(),
		"from_column_id": h.todo.String(),
		"to_column_id":   h.todo.String(),
		"after_card_id":  anchor.String(),
	})

	// The whole point. Every field that distinguishes them was absent before.
	for _, field := range []string{"card_id", "to_column_id", "after_card_id", "request_id"} {
		if moves[0][field] == moves[1][field] {
			t.Errorf("both moves logged the same %s = %#v; they are still indistinguishable",
				field, moves[0][field])
		}
	}
}

// TestARefusedMoveNamesWhichAnchorWasStale is the half that logged nothing at
// all before: writeStoreError returned early for an apiError, so a 409 left only
// the request line and its status.
func TestARefusedMoveNamesWhichAnchorWasStale(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	h := build(t, s, alice, "alpha", "one", "two")

	t.Run("an anchor that does not exist", func(t *testing.T) {
		missing := uuid.New()
		mark := s.logs.Len()

		resp := moveCard(t, s, alice.accessToken, h.cards["one"], h.todo, &missing)
		if resp.status != http.StatusConflict {
			t.Fatalf("status %d, want 409: %s", resp.status, resp.raw)
		}

		entry := entrySince(t, s, mark, "card.move.refused")
		t.Logf("refusal: %v", entry)

		fieldsAre(t, entry, map[string]any{
			"level":          "WARN",
			"reason":         "stale_after_card_id",
			"detail":         "after_card_id is not a card in that column",
			"card_id":        h.cards["one"].String(),
			"board_id":       h.board.String(),
			"from_column_id": h.todo.String(),
			"to_column_id":   h.todo.String(),
			"after_card_id":  missing.String(),
			"tenant_id":      alice.tenantID.String(),
		})

		if entry["request_id"] == "" || entry["request_id"] == nil {
			t.Errorf("the refusal does not join to the request log: %v", entry)
		}
	})

	t.Run("an anchor in a different column", func(t *testing.T) {
		two := h.cards["two"]
		mark := s.logs.Len()

		resp := moveCard(t, s, alice.accessToken, h.cards["one"], h.doing, &two)
		if resp.status != http.StatusConflict {
			t.Fatalf("status %d, want 409: %s", resp.status, resp.raw)
		}

		entry := entrySince(t, s, mark, "card.move.refused")
		t.Logf("refusal: %v", entry)

		fieldsAre(t, entry, map[string]any{
			"reason":        "stale_after_card_id",
			"after_card_id": two.String(),
			"to_column_id":  h.doing.String(),
		})
	})

	// The branch crud_bola_test.go cannot reach: a column that exists, that the
	// caller can see, on another of their own boards.
	t.Run("a column on another board", func(t *testing.T) {
		other := build(t, s, alice, "beta")
		mark := s.logs.Len()

		resp := moveCard(t, s, alice.accessToken, h.cards["one"], other.todo, nil)
		if resp.status != http.StatusConflict {
			t.Fatalf("status %d, want 409: %s", resp.status, resp.raw)
		}

		entry := entrySince(t, s, mark, "card.move.refused")
		t.Logf("refusal: %v", entry)

		fieldsAre(t, entry, map[string]any{
			"reason":       "column_on_another_board",
			"detail":       "column_id names a column on a different board",
			"card_id":      h.cards["one"].String(),
			"board_id":     h.board.String(),
			"to_board_id":  other.board.String(),
			"to_column_id": other.todo.String(),
		})
	})
}

// TestAColumnMoveIsTraceableToo covers the second endpoint end to end.
func TestAColumnMoveIsTraceableToo(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	h := build(t, s, alice, "alpha", "one")

	mark := s.logs.Len()

	if resp := moveColumnTo(t, s, alice.accessToken, h.doing, nil); resp.status != http.StatusOK {
		t.Fatalf("column move: %d %s", resp.status, resp.raw)
	}

	entry := entrySince(t, s, mark, "column.moved")
	t.Logf("column moved: %v", entry)

	fieldsAre(t, entry, map[string]any{
		"level":     "INFO",
		"column_id": h.doing.String(),
		"board_id":  h.board.String(),
		"tenant_id": alice.tenantID.String(),
	})

	if entry["after_column_id"] != nil {
		t.Errorf("after_column_id = %#v, want null for a move to the front", entry["after_column_id"])
	}

	stale := uuid.New()
	mark = s.logs.Len()

	if resp := moveColumnTo(t, s, alice.accessToken, h.doing, &stale); resp.status != http.StatusConflict {
		t.Fatalf("stale column move: %d %s", resp.status, resp.raw)
	}

	refused := entrySince(t, s, mark, "column.move.refused")
	t.Logf("column refused: %v", refused)

	fieldsAre(t, refused, map[string]any{
		"level":           "WARN",
		"reason":          "stale_after_column_id",
		"detail":          "after_column_id is not another column on this board",
		"column_id":       h.doing.String(),
		"board_id":        h.board.String(),
		"after_column_id": stale.String(),
	})
}

// TestTheRebalanceSaysItRan is the acceptance criterion ADR 0004 wanted
// evidence for. The threshold was deliberately set low "so that the renumbering
// path runs often enough to be a tested path rather than a theoretical one" —
// and until now nothing recorded that it ever did.
func TestTheRebalanceSaysItRan(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	h := build(t, s, alice, "deep", "head", "tail")

	anchor := h.cards["head"]
	mark := s.logs.Len()

	// Same driver as TestRanksAreRebalancedBeforePrecisionRuns: each nested move
	// halves the gap and adds a decimal place.
	const nested = 110

	for i := range nested {
		id := created(t, s, alice.accessToken, http.MethodPost,
			"/api/v1/columns/"+h.todo.String()+"/cards", "card",
			map[string]string{"title": fmt.Sprintf("nested-%03d", i)})

		if resp := moveCard(t, s, alice.accessToken, id, h.todo, &anchor); resp.status != http.StatusOK {
			t.Fatalf("nested move %d: %d %s", i, resp.status, resp.raw)
		}
	}

	var rebalances []map[string]any

	for _, entry := range entriesSince(t, s, mark) {
		if entry["event"] == "card.order.rebalanced" {
			rebalances = append(rebalances, entry)
		}
	}

	if len(rebalances) == 0 {
		t.Fatalf("%d nested moves logged no rebalance; either the threshold changed or the line does not fire", nested)
	}

	t.Logf("%d nested moves renumbered the column %d time(s); first: %v",
		nested, len(rebalances), rebalances[0])

	fieldsAre(t, rebalances[0], map[string]any{
		"level":     "INFO",
		"column_id": h.todo.String(),
		"board_id":  h.board.String(),
	})

	// Not every move renumbers, or the line would answer "did it run" with
	// "always" and be worth nothing.
	if len(rebalances) >= nested {
		t.Errorf("every one of %d moves logged a rebalance (%d lines); the line carries no information",
			nested, len(rebalances))
	}
}

// TestTheMoveLogPublishesNoRankAndNoTitle is the negative half, checked against
// the ranks Postgres actually assigned rather than against a guess.
//
// ADR 0004 keeps the rank out of every response; these lines are the other place
// it could leak, and they go to an aggregator.
func TestTheMoveLogPublishesNoRankAndNoTitle(t *testing.T) {
	s := newServer(t, generousLimits())
	alice := s.register(t, "alice")

	ranks := rankReader(t)

	// Titles distinctive enough that a substring match means the handler logged
	// user content and not a coincidence.
	h := build(t, s, alice, "alpha", "zzq-first-card", "zzq-second-card", "zzq-third-card")

	mark := s.logs.Len()

	// Moved *between* two siblings rather than to an end, so the rank Postgres
	// assigns is a fractional midpoint. An integral rank would make the
	// substring check below vacuous — "1" appears in half the log by accident,
	// so it has to be skipped, and then there would be nothing left to check.
	anchor := h.cards["zzq-first-card"]

	if resp := moveCard(t, s, alice.accessToken,
		h.cards["zzq-third-card"], h.todo, &anchor); resp.status != http.StatusOK {
		t.Fatalf("move: %d %s", resp.status, resp.raw)
	}

	stale := uuid.New()
	if resp := moveCard(t, s, alice.accessToken, h.cards["zzq-second-card"], h.todo, &stale); resp.status != http.StatusConflict {
		t.Fatalf("refused move: %d %s", resp.status, resp.raw)
	}

	written := s.logs.String()[mark:]

	for _, title := range []string{
		"zzq-first-card", "zzq-second-card", "zzq-third-card", "alpha project", "alpha description",
	} {
		if strings.Contains(written, title) {
			t.Errorf("the log contains user content %q:\n%s", title, written)
		}
	}

	// The actual ranks, as Postgres stored them.
	positions := ranks(h.todo)

	t.Logf("ranks in the target column after the move: %v", positions)

	fractional := 0

	for _, position := range positions {
		// A bare integer like "1" would match half the log by accident; the
		// interesting leak is the fractional midpoint a move computes, which
		// nothing else in a line looks like.
		if _, err := strconv.Atoi(position); err == nil {
			continue
		}

		fractional++

		if strings.Contains(written, position) {
			t.Errorf("the log contains the rank %q — ADR 0004 keeps it server-side:\n%s", position, written)
		}
	}

	// Without this the loop above can pass by checking nothing.
	if fractional == 0 {
		t.Fatalf("no fractional rank was produced, so the leak check proved nothing: %v", positions)
	}

	for _, entry := range entriesSince(t, s, mark) {
		for _, field := range []string{"position", "rank"} {
			if value, present := entry[field]; present {
				t.Errorf("a log line carries %q = %#v", field, value)
			}
		}
	}
}
