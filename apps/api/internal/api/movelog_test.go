package api

// What a move is required to say about itself, and what it is required not to.
//
// #93 was found by reading real logs: eighteen moves during manual verification
// produced eighteen identical lines, and the two that were refused produced no
// line of their own at all. So these tests assert the *fields*, not that
// something was logged — a test that only counted lines would have passed
// against the code that caused the issue.
//
// The negative half is as load-bearing as the positive one. TestAMoveLogsNoRank
// and TestAMoveLogsNoUserContent are what stop a later "just add the position,
// it's useful" from landing quietly: the rank is server-side only per ADR 0004,
// and a title is somebody's confidential text.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// moveRequestID is sent as an inbound X-Request-ID so the value on the domain
// lines is something the test can compare against rather than a uuid the
// middleware invented. That it is honoured at all is middleware.go's contract;
// what matters here is that the domain line carries the same one.
const moveRequestID = "req-93-move"

// logLines decodes everything the router logged, one map per line.
func (f *boardFixture) logLines(t *testing.T) []map[string]any {
	t.Helper()

	var lines []map[string]any

	for _, raw := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		if raw == "" {
			continue
		}

		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}

		lines = append(lines, line)
	}

	return lines
}

// lineFor returns the single line carrying an event name, and fails if there is
// not exactly one. Two identical move lines would be the bug this issue is
// about, so "exactly one" is part of the assertion rather than a convenience.
func (f *boardFixture) lineFor(t *testing.T, event string) map[string]any {
	t.Helper()

	var found []map[string]any

	for _, line := range f.logLines(t) {
		if line["event"] == event {
			found = append(found, line)
		}
	}

	if len(found) != 1 {
		t.Fatalf("found %d log lines with event %q, want exactly 1; log was:\n%s",
			len(found), event, f.logs.String())
	}

	return found[0]
}

func (f *boardFixture) hasEvent(t *testing.T, event string) bool {
	t.Helper()

	for _, line := range f.logLines(t) {
		if line["event"] == event {
			return true
		}
	}

	return false
}

// assertFields checks every expected key, and reports all the mismatches rather
// than the first, so a rename shows the whole shape at once.
func assertFields(t *testing.T, line map[string]any, want map[string]any) {
	t.Helper()

	for field, expected := range want {
		got, present := line[field]
		if !present {
			t.Errorf("field %q is missing; line was %v", field, line)

			continue
		}

		if got != expected {
			t.Errorf("field %q = %#v, want %#v", field, got, expected)
		}
	}
}

func (f *boardFixture) moveCard(t *testing.T, columnID uuid.UUID, after *uuid.UUID) int {
	t.Helper()

	body := map[string]any{"column_id": columnID.String(), "after_card_id": nil}
	if after != nil {
		body["after_card_id"] = after.String()
	}

	rec := f.do(t, http.MethodPost, "/api/v1/cards/"+f.alice.card.ID.String()+"/move", body,
		map[string]string{requestIDHeader: moveRequestID})

	return rec.Code
}

func (f *boardFixture) moveColumn(t *testing.T, after *uuid.UUID) int {
	t.Helper()

	body := map[string]any{"after_column_id": nil}
	if after != nil {
		body["after_column_id"] = after.String()
	}

	rec := f.do(t, http.MethodPost, "/api/v1/columns/"+f.alice.column.ID.String()+"/move", body,
		map[string]string{requestIDHeader: moveRequestID})

	return rec.Code
}

func TestASuccessfulCardMoveNamesItsSubject(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	// Into the *other* column, so from and to are different uuids. With one
	// column per tenant they would be equal, and this test would pass just as
	// happily against a handler that had the two arguments swapped.
	if status := f.moveCard(t, f.alice.otherColumn.ID, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	line := f.lineFor(t, logEventCardMoved)

	assertFields(t, line, map[string]any{
		"level":          "INFO",
		"request_id":     moveRequestID,
		"tenant_id":      f.tenantA.String(),
		"actor_user_id":  f.aliceID.String(),
		"card_id":        f.alice.card.ID.String(),
		"board_id":       f.alice.board.ID.String(),
		"from_column_id": f.alice.column.ID.String(),
		"to_column_id":   f.alice.otherColumn.ID.String(),
	})

	// Explicitly null rather than absent: "first in the column" is a position no
	// sibling's id can name, and a client reading these back has to be able to
	// tell it from a field nobody set. Same rule as cardMovedPayload.
	after, present := line["after_card_id"]
	if !present {
		t.Errorf("after_card_id is absent; it must be an explicit null for a move to first")
	}

	if after != nil {
		t.Errorf("after_card_id = %#v, want null", after)
	}
}

func TestASuccessfulColumnMoveNamesItsSubject(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	if status := f.moveColumn(t, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	line := f.lineFor(t, logEventColumnMoved)

	assertFields(t, line, map[string]any{
		"level":         "INFO",
		"request_id":    moveRequestID,
		"tenant_id":     f.tenantA.String(),
		"actor_user_id": f.aliceID.String(),
		"column_id":     f.alice.column.ID.String(),
		"board_id":      f.alice.board.ID.String(),
	})

	if after, present := line["after_column_id"]; !present || after != nil {
		t.Errorf("after_column_id = %#v (present=%v), want an explicit null", after, present)
	}
}

// TestARefusedCardMoveNamesTheStaleAnchor is the half of #93 that motivated it.
// Before this, a 409 left only the INFO request line and its status, so "why was
// this user's drag refused" was unanswerable.
func TestARefusedCardMoveNamesTheStaleAnchor(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	// Any non-null anchor is stale in this fixture — each tenant has one card,
	// so no id names a *second* card in the target column.
	stale := uuid.New()

	if status := f.moveCard(t, f.alice.otherColumn.ID, &stale); status != http.StatusConflict {
		t.Fatalf("move returned %d, want 409", status)
	}

	line := f.lineFor(t, logEventCardMoveRefused)

	assertFields(t, line, map[string]any{
		"level":         "WARN",
		"request_id":    moveRequestID,
		"tenant_id":     f.tenantA.String(),
		"actor_user_id": f.aliceID.String(),
		"reason":        reasonStaleCardAnchor,
		"detail":        "after_card_id is not a card in that column",
		"card_id":       f.alice.card.ID.String(),
		"board_id":      f.alice.board.ID.String(),

		// A refusal describes what was *asked for*, not what exists: the card
		// is still in `column`, and `otherColumn` is where it was headed.
		"from_column_id": f.alice.column.ID.String(),
		"to_column_id":   f.alice.otherColumn.ID.String(),

		// The point of the whole line: which anchor the client named.
		"after_card_id": stale.String(),
	})

	if f.hasEvent(t, logEventCardMoved) {
		t.Errorf("a refused move logged card.moved; log was:\n%s", f.logs.String())
	}
}

func TestARefusedColumnMoveNamesTheStaleAnchor(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	stale := uuid.New()

	if status := f.moveColumn(t, &stale); status != http.StatusConflict {
		t.Fatalf("move returned %d, want 409", status)
	}

	line := f.lineFor(t, logEventColumnMoveRefused)

	assertFields(t, line, map[string]any{
		"level":           "WARN",
		"request_id":      moveRequestID,
		"tenant_id":       f.tenantA.String(),
		"actor_user_id":   f.aliceID.String(),
		"reason":          reasonStaleColumnAnchor,
		"detail":          "after_column_id is not another column on this board",
		"column_id":       f.alice.column.ID.String(),
		"board_id":        f.alice.board.ID.String(),
		"after_column_id": stale.String(),
	})

	if f.hasEvent(t, logEventColumnMoved) {
		t.Errorf("a refused move logged column.moved; log was:\n%s", f.logs.String())
	}
}

// TestAMoveThatAnswers404LogsNothing pins the pairing, not the cross-board 409.
//
// bob's column resolves to nothing inside alice's transaction, so this is a 404
// — "not yours" and "never existed" are the same answer under row-level
// security. The cross-board 409 needs a second board in the *same* tenant, which
// this fixture does not have and TestARefusedMoveNamesWhichAnchorWasStale in
// movelog_integration_test.go does.
func TestAMoveThatAnswers404LogsNothing(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	if status := f.moveCard(t, f.bob.column.ID, nil); status != http.StatusNotFound {
		t.Fatalf("move returned %d, want 404", status)
	}

	if f.hasEvent(t, logEventCardMoveRefused) {
		t.Errorf("a 404 logged a refusal; log was:\n%s", f.logs.String())
	}

	if f.hasEvent(t, logEventCardMoved) {
		t.Errorf("a 404 logged a move; log was:\n%s", f.logs.String())
	}
}

func TestARebalancingMoveSaysItRan(t *testing.T) {
	t.Parallel()

	t.Run("cards", func(t *testing.T) {
		t.Parallel()

		f := newBoardFixture(t)
		f.store.rebalanceEveryMove()

		if status := f.moveCard(t, f.alice.column.ID, nil); status != http.StatusOK {
			t.Fatalf("move returned %d, want 200", status)
		}

		assertFields(t, f.lineFor(t, logEventCardOrderRebalanced), map[string]any{
			"level":      "INFO",
			"request_id": moveRequestID,
			"board_id":   f.alice.board.ID.String(),
			"column_id":  f.alice.column.ID.String(),
		})

		// The line has to mean the renumbering happened, not that the branch was
		// entered — otherwise it is a worse signal than none.
		if parents := f.store.rebalancedParents(); len(parents) != 1 || parents[0] != f.alice.column.ID {
			t.Errorf("rebalanced %v, want exactly [%v]", parents, f.alice.column.ID)
		}
	})

	t.Run("columns", func(t *testing.T) {
		t.Parallel()

		f := newBoardFixture(t)
		f.store.rebalanceEveryMove()

		if status := f.moveColumn(t, nil); status != http.StatusOK {
			t.Fatalf("move returned %d, want 200", status)
		}

		assertFields(t, f.lineFor(t, logEventColumnOrderRebalanced), map[string]any{
			"level":      "INFO",
			"request_id": moveRequestID,
			"board_id":   f.alice.board.ID.String(),
		})

		if parents := f.store.rebalancedParents(); len(parents) != 1 || parents[0] != f.alice.board.ID {
			t.Errorf("rebalanced %v, want exactly [%v]", parents, f.alice.board.ID)
		}
	})
}

// TestAMoveThatDoesNotRebalanceSaysNothing keeps the rebalance line meaningful:
// a line on every move would answer "did the renumbering path run" with "yes",
// always, which is the question ADR 0004 wanted evidence for.
func TestAMoveThatDoesNotRebalanceSaysNothing(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	if status := f.moveCard(t, f.alice.column.ID, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	if f.hasEvent(t, logEventCardOrderRebalanced) {
		t.Errorf("a move that did not rebalance logged one; log was:\n%s", f.logs.String())
	}
}

// TestARolledBackMoveLogsNothing is why the success line is emitted after
// tenantScopedPublish returns rather than inside the transaction. A line saying
// a card moved, for a transaction that did not commit, is worse than no line:
// it is the log agreeing with a client that is wrong.
func TestARolledBackMoveLogsNothing(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)
	f.store.rebalanceEveryMove()
	f.store.breakCommit(errors.New("commit failed"))

	if status := f.moveCard(t, f.alice.otherColumn.ID, nil); status != http.StatusInternalServerError {
		t.Fatalf("move returned %d, want 500", status)
	}

	for _, event := range []string{logEventCardMoved, logEventCardOrderRebalanced} {
		if f.hasEvent(t, event) {
			t.Errorf("a rolled-back move logged %q; log was:\n%s", event, f.logs.String())
		}
	}
}

// TestARolledBackColumnMoveLogsNothing is the same guarantee on the other
// endpoint. Stated separately because the two handlers log from two call sites,
// and a copy-paste that moved one line above tenantScopedPublish would only
// break one of them.
func TestARolledBackColumnMoveLogsNothing(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)
	f.store.rebalanceEveryMove()
	f.store.breakCommit(errors.New("commit failed"))

	if status := f.moveColumn(t, nil); status != http.StatusInternalServerError {
		t.Fatalf("move returned %d, want 500", status)
	}

	for _, event := range []string{logEventColumnMoved, logEventColumnOrderRebalanced} {
		if f.hasEvent(t, event) {
			t.Errorf("a rolled-back move logged %q; log was:\n%s", event, f.logs.String())
		}
	}
}

// TestAMoveStillLogsWhenTheBroadcastFails pins the order of the two things a
// committed move does. ADR 0005 accepts a failed publish and keeps the write, so
// the log must describe the write that happened — a move recorded only when
// Redis was reachable would be worst exactly when an operator needs it.
func TestAMoveStillLogsWhenTheBroadcastFails(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)
	f.events.failWith = errors.New("redis is unreachable")

	if status := f.moveCard(t, f.alice.otherColumn.ID, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	assertFields(t, f.lineFor(t, logEventCardMoved), map[string]any{
		"card_id":      f.alice.card.ID.String(),
		"to_column_id": f.alice.otherColumn.ID.String(),
	})

	// And the publish failure is still its own line, not swallowed by this one.
	if !f.hasEvent(t, "realtime.publish.failed") {
		t.Errorf("the failed broadcast logged nothing; log was:\n%s", f.logs.String())
	}
}

// TestAMoveLogsNoRank guards ADR 0004's "the rank is server-side only" in the
// place it is easiest to break by accident. A rank in a log is a lower bar than
// a rank in a response, but these lines ship to an aggregator, and there is no
// question a reader can answer with a numeric midpoint that the anchor does not
// answer better.
func TestAMoveLogsNoRank(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)
	f.store.rebalanceEveryMove()

	if status := f.moveCard(t, f.alice.column.ID, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	if status := f.moveColumn(t, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	forbidden := []string{"position", "rank", "new_position", "new_rank"}

	for _, line := range f.logLines(t) {
		for _, field := range forbidden {
			if value, present := line[field]; present {
				t.Errorf("log line carries %q = %#v; the rank is not published — ADR 0004", field, value)
			}
		}
	}
}

// TestAMoveLogsNoUserContent is the same guard for titles and names. The ids are
// what reconstruct an order; the text is somebody's, and may be confidential.
func TestAMoveLogsNoUserContent(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	if status := f.moveCard(t, f.alice.column.ID, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	stale := uuid.New()
	if status := f.moveCard(t, f.alice.column.ID, &stale); status != http.StatusConflict {
		t.Fatalf("move returned %d, want 409", status)
	}

	if status := f.moveColumn(t, nil); status != http.StatusOK {
		t.Fatalf("move returned %d, want 200", status)
	}

	// The fixture's markers appear in the card title, the card description and
	// the column name and nowhere else, so finding one in the log means a
	// handler put user content there.
	output := f.logs.String()

	for _, content := range []string{
		f.alice.card.Title, f.alice.card.Description,
		f.alice.column.Name, f.alice.otherColumn.Name, f.alice.board.Name,
	} {
		if strings.Contains(output, content) {
			t.Errorf("log contains user content %q; ids only — see movelog.go\nlog was:\n%s", content, output)
		}
	}
}

// TestTwoMovesAreDistinguishable is the issue's actual complaint, reduced to an
// assertion: the same endpoint hit twice produced two lines that differ in the
// only way a reader cares about.
func TestTwoMovesAreDistinguishable(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	if status := f.moveCard(t, f.alice.column.ID, nil); status != http.StatusOK {
		t.Fatalf("first move returned %d, want 200", status)
	}

	stale := uuid.New()
	if status := f.moveCard(t, f.alice.column.ID, &stale); status != http.StatusConflict {
		t.Fatalf("second move returned %d, want 409", status)
	}

	// The request log cannot tell them apart beyond the status: both are the
	// route template. This is the "before" the domain lines exist to fix, and
	// asserting it keeps the comparison honest if the request log ever changes.
	var requestPaths []any

	for _, line := range f.logLines(t) {
		if line["msg"] == "http request" {
			requestPaths = append(requestPaths, line["path"])
		}
	}

	if len(requestPaths) != 2 || requestPaths[0] != requestPaths[1] {
		t.Fatalf("expected two request lines with the same route template, got %v", requestPaths)
	}

	moved := f.lineFor(t, logEventCardMoved)
	refused := f.lineFor(t, logEventCardMoveRefused)

	if moved["after_card_id"] == refused["after_card_id"] {
		t.Errorf("both moves logged the same anchor %#v", moved["after_card_id"])
	}

	if refused["after_card_id"] != stale.String() {
		t.Errorf("refusal named anchor %#v, want %q", refused["after_card_id"], stale)
	}
}

// TestAnUnannotatedConflictIsStillLogged pins the fallback in logRefusal. The
// guarantee is "no 409 is silent", and it must not depend on the handler that
// raised it having remembered to call loggedAs — that dependency is exactly what
// made the 409 silent in the first place.
func TestAnUnannotatedConflictIsStillLogged(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	// A handler that raises a bare conflict, mounted on the real router's
	// middleware so the request id and the principal are in place.
	f.router.POST("/api/v1/test/bare-conflict",
		requireAuth(discardLogger(), f.issuer),
		func(c *gin.Context) {
			_, _ = tenantScoped(c, f.logger, f.store, "test.bare.failed",
				func(context.Context, store.Querier) (struct{}, error) {
					return struct{}{}, conflict("something disagreed")
				})
		})

	rec := f.do(t, http.MethodPost, "/api/v1/test/bare-conflict", nil,
		map[string]string{requestIDHeader: moveRequestID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("returned %d, want 409", rec.Code)
	}

	assertFields(t, f.lineFor(t, "test.bare.failed"), map[string]any{
		"level":      "WARN",
		"request_id": moveRequestID,
		"tenant_id":  f.tenantA.String(),
		"detail":     "something disagreed",
	})
}

// TestARefusalThatCarriedAnotherFailureSaysSo covers the residue of the same
// defect one level down.
//
// store.WithTenant joins a failed rollback onto the callback's error rather than
// replacing it, and errors.As still finds the apiError inside that join — so the
// status is right, the refusal line is right, and a discarded pool connection
// used to vanish completely. The 404 case is the worse one, because a 404 writes
// no line of its own at all.
func TestARefusalThatCarriedAnotherFailureSaysSo(t *testing.T) {
	t.Parallel()

	rollback := errors.New("rolling back tenant transaction: conn busy")

	findFailureLine := func(t *testing.T, f *boardFixture) map[string]any {
		t.Helper()

		for _, line := range f.logLines(t) {
			if line["msg"] == "a refusal carried an additional failure" {
				return line
			}
		}

		t.Fatalf("the joined failure was not logged; log was:\n%s", f.logs.String())

		return nil
	}

	t.Run("behind a 409", func(t *testing.T) {
		t.Parallel()

		f := newBoardFixture(t)
		f.store.joinRollbackFailure(rollback)

		stale := uuid.New()
		if status := f.moveCard(t, f.alice.otherColumn.ID, &stale); status != http.StatusConflict {
			t.Fatalf("move returned %d, want 409", status)
		}

		line := findFailureLine(t, f)

		assertFields(t, line, map[string]any{
			"level":      "ERROR",
			"request_id": moveRequestID,
			"status":     float64(http.StatusConflict),
		})

		if detail, _ := line["error"].(string); !strings.Contains(detail, "conn busy") {
			t.Errorf("error = %#v, want it to name the rollback failure", line["error"])
		}

		// The refusal itself is still reported; this is an extra line, not a
		// replacement.
		if !f.hasEvent(t, logEventCardMoveRefused) {
			t.Errorf("the refusal line was lost; log was:\n%s", f.logs.String())
		}
	})

	t.Run("behind a 404", func(t *testing.T) {
		t.Parallel()

		f := newBoardFixture(t)
		f.store.joinRollbackFailure(rollback)

		if status := f.moveCard(t, f.bob.column.ID, nil); status != http.StatusNotFound {
			t.Fatalf("move returned %d, want 404", status)
		}

		assertFields(t, findFailureLine(t, f), map[string]any{
			"level":  "ERROR",
			"status": float64(http.StatusNotFound),
		})
	})
}

// TestAnOrdinaryRefusalLogsNoFailureLine keeps the test above honest: the ERROR
// line must be the exception, not something every 409 emits.
func TestAnOrdinaryRefusalLogsNoFailureLine(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	stale := uuid.New()
	if status := f.moveCard(t, f.alice.otherColumn.ID, &stale); status != http.StatusConflict {
		t.Fatalf("move returned %d, want 409", status)
	}

	for _, line := range f.logLines(t) {
		if line["msg"] == "a refusal carried an additional failure" {
			t.Errorf("a plain refusal logged a failure line: %v", line)
		}
	}
}

// TestANotFoundIsNotLoggedAsARefusal pins the other half of that decision. A 404
// is the ordinary answer for an id naming nothing the caller can see — including
// every id in another tenant — and logging one per stale tab would be volume
// rather than signal. Deliberate, and revisited in its own issue if ever.
func TestANotFoundIsNotLoggedAsARefusal(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	rec := f.do(t, http.MethodGet, "/api/v1/cards/"+uuid.New().String(), nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("returned %d, want 404", rec.Code)
	}

	for _, line := range f.logLines(t) {
		if line["msg"] == "refusing a write that disagrees with the board" {
			t.Errorf("a 404 was logged as a refusal: %v", line)
		}
	}
}

// TestTheMoveLogEventNamesAreStable is a tripwire, not a tautology. These names
// are the vocabulary an operator's saved searches and any future alert are
// written against, so renaming one is a breaking change to something outside
// this repository and should not be possible to do by accident.
func TestTheMoveLogEventNamesAreStable(t *testing.T) {
	t.Parallel()

	names := map[string]string{
		"logEventCardMoved":             logEventCardMoved,
		"logEventCardMoveRefused":       logEventCardMoveRefused,
		"logEventCardOrderRebalanced":   logEventCardOrderRebalanced,
		"logEventColumnMoved":           logEventColumnMoved,
		"logEventColumnMoveRefused":     logEventColumnMoveRefused,
		"logEventColumnOrderRebalanced": logEventColumnOrderRebalanced,
	}

	want := map[string]string{
		"logEventCardMoved":             "card.moved",
		"logEventCardMoveRefused":       "card.move.refused",
		"logEventCardOrderRebalanced":   "card.order.rebalanced",
		"logEventColumnMoved":           "column.moved",
		"logEventColumnMoveRefused":     "column.move.refused",
		"logEventColumnOrderRebalanced": "column.order.rebalanced",
	}

	seen := map[string]string{}

	for constant, name := range names {
		if name != want[constant] {
			t.Errorf("%s = %q, want %q — renaming this breaks saved searches outside this repo",
				constant, name, want[constant])
		}

		if other, collides := seen[name]; collides {
			t.Errorf("%s and %s are both %q; two events that cannot be told apart", constant, other, name)
		}

		seen[name] = constant
	}
}

// There is deliberately no test asserting that logEventCardMoved equals
// eventCardMoved. They are spelled the same today, and movelog.go explains why
// that is convenient — but pinning it would manufacture exactly the pressure
// that comment exists to resist: a future version bump on the realtime envelope
// would turn CI red, and the cheapest way to green would be to rename the log
// constants to chase the wire ones. The agreement is an observation, not a
// constraint, so it lives in the comment and nowhere else.
