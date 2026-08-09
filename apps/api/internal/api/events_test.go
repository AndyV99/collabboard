package api

// The realtime event contract, tested where it is decided.
//
// crud_bola_test.go proves that no event crosses a tenant or a board boundary.
// This file proves the other three things #45 is about:
//
//  1. every write that should announce itself does, with the payload the
//     frontend is going to be built against;
//  2. a write that does not commit announces nothing, which is the one failure
//     that would be worse than not broadcasting at all;
//  3. a broadcast that fails does not fail the write, and the response waits for
//     the broadcast — which is what makes the ordering guarantee in
//     docs/adr/0005-realtime-event-delivery.md true.
//
// It reuses crud_bola_test.go's fixture rather than building a second one. That
// is deliberate: the fake store there already models the RLS behaviour every
// handler depends on, and a parallel harness would be a second description of
// the same world, free to drift.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// recordingPublisher is an [EventPublisher] that keeps what it was handed.
//
// It records the [BoardEvent] rather than a rendered frame, because the tenant
// and the actor are filled in by publishBoardEvent and are exactly what the
// cross-tenant assertions are about.
type recordingPublisher struct {
	mu     sync.Mutex
	events []BoardEvent

	// failWith makes every publish fail, for the "the write still stands" case.
	failWith error

	// block, when set, holds the publish until it is closed. It is how "the
	// response waits for the broadcast" is observed.
	block chan struct{}

	// entered is closed by the first publish that blocks.
	entered chan struct{}

	// What the publish's own context looked like, for the two guarantees the
	// ADR makes about it: detached from the request's cancellation, and bounded.
	ctxErr      error
	ctxDeadline bool
	ctxBudget   time.Duration
}

func (p *recordingPublisher) PublishBoardEvent(ctx context.Context, event BoardEvent) error {
	deadline, hasDeadline := ctx.Deadline()

	p.mu.Lock()
	p.events = append(p.events, event)
	p.ctxErr = ctx.Err()
	p.ctxDeadline = hasDeadline
	p.ctxBudget = time.Until(deadline)
	failWith := p.failWith
	block, entered := p.block, p.entered
	p.mu.Unlock()

	if block != nil {
		close(entered)
		<-block
	}

	return failWith
}

func (p *recordingPublisher) published() []BoardEvent {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Clone(p.events)
}

func (p *recordingPublisher) publishedFrom(from int) []BoardEvent {
	published := p.published()
	if from > len(published) {
		return nil
	}

	return published[from:]
}

// only returns the single event a write should have produced, and fails if the
// count is anything else. A write that published twice is as wrong as one that
// published nothing.
func (p *recordingPublisher) only(t *testing.T) BoardEvent {
	t.Helper()

	published := p.published()
	if len(published) != 1 {
		t.Fatalf("published %d events, want exactly 1: %+v", len(published), published)
	}

	return published[0]
}

// payloadOf renders an event's payload the way a client will receive it, so the
// assertions below are about JSON field names rather than Go struct fields. The
// wire shape is the part that hardens into the frontend.
func payloadOf(t *testing.T, event BoardEvent) map[string]any {
	t.Helper()

	encoded, err := json.Marshal(event.Payload)
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}

	t.Logf("%s payload: %s", event.Type, encoded)

	return decoded
}

// TestEveryBoardWritePublishesItsEvent is the wire contract, stated once.
//
// The `want` fields are the shape the frontend will be written against, so a
// change to any of them is a change a client has to be redeployed for — which is
// exactly why they are asserted here rather than left implied by the structs.
func TestEveryBoardWritePublishesItsEvent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string

		method string
		path   func(f *boardFixture) string
		body   func(f *boardFixture) any

		wantStatus int
		wantType   string

		// assert checks the decoded payload.
		assert func(t *testing.T, f *boardFixture, payload map[string]any)
	}{
		{
			name:   "creating a card",
			method: http.MethodPost,
			path:   func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() + "/cards" },
			body:   func(*boardFixture) any { return map[string]string{"title": "a new card"} },

			wantStatus: http.StatusCreated,
			wantType:   "card.created",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				card := object(t, payload, "card")
				requireField(t, card, "id", f.alice.card.ID.String())
				requireField(t, card, "column_id", f.alice.column.ID.String())

				// No anchor: a created card is appended to the end of its
				// column, and that is the whole placement rule for it.
				if _, ok := payload["after_card_id"]; ok {
					t.Error("card.created carries an anchor; a create is an append and has none")
				}
			},
		},
		{
			name:   "updating a card",
			method: http.MethodPatch,
			path:   func(f *boardFixture) string { return "/api/v1/cards/" + f.alice.card.ID.String() },
			body:   func(*boardFixture) any { return map[string]string{"title": "retitled"} },

			wantStatus: http.StatusOK,
			wantType:   "card.updated",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				card := object(t, payload, "card")
				requireField(t, card, "id", f.alice.card.ID.String())

				// The whole card, not a field diff: a client replaces its copy
				// and has no merge to get wrong.
				for _, field := range []string{"board_id", "column_id", "title", "description", "created_at", "updated_at"} {
					if _, ok := card[field]; !ok {
						t.Errorf("card.updated payload has no %q; it must carry the same shape as the REST body", field)
					}
				}
			},
		},
		{
			name:   "moving a card to the front of a column",
			method: http.MethodPost,
			path:   func(f *boardFixture) string { return "/api/v1/cards/" + f.alice.card.ID.String() + "/move" },
			body: func(f *boardFixture) any {
				return map[string]any{"column_id": f.alice.column.ID.String(), "after_card_id": nil}
			},

			wantStatus: http.StatusOK,
			wantType:   "card.moved",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				card := object(t, payload, "card")
				requireField(t, card, "column_id", f.alice.column.ID.String())

				// Where it came from, so a client holding one list per column
				// knows which list to take it out of without scanning them all.
				requireField(t, payload, "from_column_id", f.alice.column.ID.String())

				// Present *and* null. Absent would mean "no placement
				// information"; null means "first in the column", and a client
				// has to be able to tell those apart.
				anchor, ok := payload["after_card_id"]
				if !ok {
					t.Fatal("card.moved has no after_card_id; a client cannot place the card without it")
				}

				if anchor != nil {
					t.Errorf("after_card_id = %v, want null for a move to the front", anchor)
				}

				// The rank is never published. ADR 0004 is explicit that a
				// client must not be able to depend on a number renumbering will
				// change.
				if _, ok := card["position"]; ok {
					t.Error("the card body in an event carries a position; ADR 0004 says no endpoint publishes one")
				}
			},
		},
		{
			name:   "deleting a card",
			method: http.MethodDelete,
			path:   func(f *boardFixture) string { return "/api/v1/cards/" + f.alice.card.ID.String() },

			wantStatus: http.StatusNoContent,
			wantType:   "card.deleted",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				requireField(t, payload, "card_id", f.alice.card.ID.String())
				requireField(t, payload, "column_id", f.alice.column.ID.String())

				// Ids only. There is nothing left to render, and a body would
				// invite a client to display a card that no longer exists.
				if _, ok := payload["card"]; ok {
					t.Error("card.deleted carries a card body")
				}
			},
		},
		{
			name:   "creating a column",
			method: http.MethodPost,
			path:   func(f *boardFixture) string { return "/api/v1/boards/" + f.alice.board.ID.String() + "/columns" },
			body:   func(*boardFixture) any { return map[string]string{"name": "a new column"} },

			wantStatus: http.StatusCreated,
			wantType:   "column.created",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				column := object(t, payload, "column")
				requireField(t, column, "board_id", f.alice.board.ID.String())
			},
		},
		{
			name:   "renaming a column",
			method: http.MethodPatch,
			path:   func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() },
			body:   func(*boardFixture) any { return map[string]string{"name": "renamed"} },

			wantStatus: http.StatusOK,
			wantType:   "column.updated",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				object(t, payload, "column")
			},
		},
		{
			name:   "moving a column",
			method: http.MethodPost,
			path:   func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() + "/move" },
			body:   func(*boardFixture) any { return map[string]any{"after_column_id": nil} },

			wantStatus: http.StatusOK,
			wantType:   "column.moved",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				object(t, payload, "column")

				anchor, ok := payload["after_column_id"]
				if !ok || anchor != nil {
					t.Errorf("after_column_id = %v (present: %t), want an explicit null", anchor, ok)
				}
			},
		},
		{
			name:   "deleting a column",
			method: http.MethodDelete,
			path:   func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() },

			wantStatus: http.StatusNoContent,
			wantType:   "column.deleted",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				requireField(t, payload, "column_id", f.alice.column.ID.String())
			},
		},
		{
			name:   "renaming a board",
			method: http.MethodPatch,
			path:   func(f *boardFixture) string { return "/api/v1/boards/" + f.alice.board.ID.String() },
			body:   func(*boardFixture) any { return map[string]string{"name": "renamed"} },

			wantStatus: http.StatusOK,
			wantType:   "board.updated",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				object(t, payload, "board")
			},
		},
		{
			name:   "deleting a board",
			method: http.MethodDelete,
			path:   func(f *boardFixture) string { return "/api/v1/boards/" + f.alice.board.ID.String() },

			wantStatus: http.StatusNoContent,
			wantType:   "board.deleted",
			assert: func(t *testing.T, f *boardFixture, payload map[string]any) {
				requireField(t, payload, "board_id", f.alice.board.ID.String())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newBoardFixture(t)

			var body any
			if tc.body != nil {
				body = tc.body(f)
			}

			rec := f.do(t, tc.method, tc.path(f), body, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			event := f.events.only(t)

			if event.Type != tc.wantType {
				t.Errorf("event type = %q, want %q", event.Type, tc.wantType)
			}

			if event.BoardID != f.alice.board.ID {
				t.Errorf("event board = %s, want %s", event.BoardID, f.alice.board.ID)
			}

			if event.ActorID != f.aliceID || event.TenantID != f.tenantA {
				t.Errorf("event actor/tenant = %s/%s, want %s/%s",
					event.ActorID, event.TenantID, f.aliceID, f.tenantA)
			}

			tc.assert(t, f, payloadOf(t, event))
		})
	}
}

// TestTheWritesThatDeliberatelyPublishNothing pins the other half of the
// decision.
//
// A realtime room is a board. Everything above a board — a project, and the
// creation of the board itself — has no room to be announced in, so it is not a
// missing feature that these are silent, it is the addressing model. If a
// project- or tenant-scoped room ever exists (#52), this test is where the
// decision changes.
func TestTheWritesThatDeliberatelyPublishNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		path   func(f *boardFixture) string
		body   any
		want   int
	}{
		{
			name: "creating a project", method: http.MethodPost, want: http.StatusCreated,
			path: func(*boardFixture) string { return "/api/v1/projects" },
			body: map[string]string{"name": "a project"},
		},
		{
			name: "renaming a project", method: http.MethodPatch, want: http.StatusOK,
			path: func(f *boardFixture) string { return "/api/v1/projects/" + f.alice.project.ID.String() },
			body: map[string]string{"name": "renamed"},
		},
		{
			name: "archiving a project", method: http.MethodPost, want: http.StatusOK,
			path: func(f *boardFixture) string { return "/api/v1/projects/" + f.alice.project.ID.String() + "/archive" },
		},
		{
			name: "creating a board", method: http.MethodPost, want: http.StatusCreated,
			path: func(f *boardFixture) string { return "/api/v1/projects/" + f.alice.project.ID.String() + "/boards" },
			body: map[string]string{"name": "a board"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newBoardFixture(t)

			rec := f.do(t, tc.method, tc.path(f), tc.body, nil)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}

			if published := f.events.published(); len(published) != 0 {
				t.Errorf("%s published %+v; a board room cannot carry it", tc.name, published)
			}
		})
	}
}

// errCommitFailed stands in for a transaction that did not survive.
var errCommitFailed = errors.New("committing tenant transaction: connection reset")

// TestARolledBackWriteBroadcastsNothing is the correctness claim at the centre
// of #45.
//
// Publishing from inside the transaction would announce a move that a rollback
// then un-did, and every open browser on the board would show a card that does
// not exist until somebody reloads. Worse than silence, because it is wrong
// rather than stale.
//
// The failure is injected at *commit*, not inside the callback, and that is the
// point: a handler that published as its last statement inside WithTenant would
// have done everything right and still be wrong. Only publishing after
// WithTenant returns survives this.
func TestARolledBackWriteBroadcastsNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		path   func(f *boardFixture) string
		body   func(f *boardFixture) any
	}{
		{
			name: "a card move", method: http.MethodPost,
			path: func(f *boardFixture) string { return "/api/v1/cards/" + f.alice.card.ID.String() + "/move" },
			body: func(f *boardFixture) any {
				return map[string]any{"column_id": f.alice.column.ID.String(), "after_card_id": nil}
			},
		},
		{
			name: "a card create", method: http.MethodPost,
			path: func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() + "/cards" },
			body: func(*boardFixture) any { return map[string]string{"title": "never committed"} },
		},
		{
			name: "a card update", method: http.MethodPatch,
			path: func(f *boardFixture) string { return "/api/v1/cards/" + f.alice.card.ID.String() },
			body: func(*boardFixture) any { return map[string]string{"title": "never committed"} },
		},
		{
			name: "a card delete", method: http.MethodDelete,
			path: func(f *boardFixture) string { return "/api/v1/cards/" + f.alice.card.ID.String() },
		},
		{
			name: "a column create", method: http.MethodPost,
			path: func(f *boardFixture) string { return "/api/v1/boards/" + f.alice.board.ID.String() + "/columns" },
			body: func(*boardFixture) any { return map[string]string{"name": "never committed"} },
		},
		{
			name: "a column update", method: http.MethodPatch,
			path: func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() },
			body: func(*boardFixture) any { return map[string]string{"name": "never committed"} },
		},
		{
			name: "a column move", method: http.MethodPost,
			path: func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() + "/move" },
			body: func(*boardFixture) any { return map[string]any{"after_column_id": nil} },
		},
		{
			name: "a column delete", method: http.MethodDelete,
			path: func(f *boardFixture) string { return "/api/v1/columns/" + f.alice.column.ID.String() },
		},
		{
			name: "a board update", method: http.MethodPatch,
			path: func(f *boardFixture) string { return "/api/v1/boards/" + f.alice.board.ID.String() },
			body: func(*boardFixture) any { return map[string]string{"name": "never committed"} },
		},
		{
			// The one describe function that is not handed a row: it is handed
			// the id the transaction proved was deletable. If the commit fails,
			// that proof is worthless and the event must not go out anyway.
			name: "a board delete", method: http.MethodDelete,
			path: func(f *boardFixture) string { return "/api/v1/boards/" + f.alice.board.ID.String() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newBoardFixture(t)

			// Everything the callback does succeeds; the commit is what fails.
			f.store.breakCommit(errCommitFailed)

			var body any
			if tc.body != nil {
				body = tc.body(f)
			}

			rec := f.do(t, tc.method, tc.path(f), body, nil)

			t.Logf("%s with a failing commit -> %d %s", tc.name, rec.Code, truncate(rec.Body.String()))

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500: the client must not be told a write happened", rec.Code)
			}

			if published := f.events.published(); len(published) != 0 {
				t.Fatalf("a write that did not commit published %d event(s): %+v\n"+
					"every client on the board would now show a change the database never made",
					len(published), published)
			}

			t.Log("the transaction did not commit and nothing was broadcast")
		})
	}
}

// TestARolledBackWriteAssertionHasTeeth shows the assertion above catching the
// mistake it exists for.
//
// It publishes from *inside* the transaction — the shape a handler naturally
// grows into if the publisher is in scope there — and requires the event to
// escape. If this ever stops leaking, the test above is measuring nothing.
func TestARolledBackWriteAssertionHasTeeth(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)
	f.store.breakCommit(errCommitFailed)

	// The mistake, written out. It reads perfectly well: read the row, announce
	// what happened to it, return. It is only wrong because the transaction is
	// still open on the last line.
	err := f.store.WithTenant(t.Context(), f.tenantA, func(ctx context.Context, q store.Querier) error {
		card, err := q.GetCard(ctx, f.alice.card.ID)
		if err != nil {
			return err
		}

		return f.events.PublishBoardEvent(ctx, BoardEvent{
			TenantID: f.tenantA,
			ActorID:  f.aliceID,
			BoardID:  card.BoardID,
			Type:     eventCardMoved,
			Payload:  cardMovedPayload{Card: newCardBody(card)},
		})
	})

	if !errors.Is(err, errCommitFailed) {
		t.Fatalf("the transaction was supposed to fail at commit; got %v", err)
	}

	published := f.events.published()

	t.Logf("the transaction rolled back and %d event(s) had already gone out: %+v", len(published), published)

	if len(published) != 1 {
		t.Fatal("publishing from inside the transaction did not leak an event; " +
			"TestARolledBackWriteBroadcastsNothing cannot detect one either")
	}

	t.Log("confirmed: an event published inside the transaction outlives the rollback, " +
		"and the assertion above is what catches it")
}

// object reads a nested JSON object out of a payload.
func object(t *testing.T, payload map[string]any, key string) map[string]any {
	t.Helper()

	value, ok := payload[key].(map[string]any)
	if !ok {
		t.Fatalf("payload has no %q object: %+v", key, payload)
	}

	return value
}

func requireField(t *testing.T, object map[string]any, field, want string) {
	t.Helper()

	got, ok := object[field].(string)
	if !ok {
		t.Fatalf("no %q in %+v", field, object)
	}

	if got != want {
		t.Errorf("%s = %s, want %s", field, got, want)
	}
}

// TestAFailedBroadcastDoesNotFailTheWrite is the other half of the ADR 0005
// decision: the event is best effort and the write is not.
//
// A client that got a 500 for a card move it actually performed would retry it,
// which is a worse outcome than a client whose board is briefly stale — and the
// board is not even stale for long, because every client re-fetches when it
// subscribes.
func TestAFailedBroadcastDoesNotFailTheWrite(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)
	f.events.failWith = errors.New("redis: connection refused")

	rec := f.do(t, http.MethodPost, "/api/v1/cards/"+f.alice.card.ID.String()+"/move",
		map[string]any{"column_id": f.alice.column.ID.String(), "after_card_id": nil}, nil)

	t.Logf("a card move with an unreachable broker -> %d %s", rec.Code, truncate(rec.Body.String()))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a broadcast failure must not roll a committed write back", rec.Code)
	}

	if len(f.events.published()) != 1 {
		t.Error("the publish was not attempted")
	}
}

// TestTheBroadcastOutlivesTheRequestThatCausedIt covers the two properties of
// the publish context that ADR 0005 promises and that nothing else would notice
// the loss of.
//
// The common way a request context is cancelled is a client that hung up. By
// then the write is committed and durable, so inheriting that cancellation would
// mean one client closing its laptop is the reason every *other* client on the
// board never hears about its last card move. And the detached context still has
// to be bounded, or an unreachable Redis would hold the request open for as long
// as the driver felt like.
func TestTheBroadcastOutlivesTheRequestThatCausedIt(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)

	// A request whose context is already cancelled — the client is gone.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	body, err := json.Marshal(map[string]any{"column_id": f.alice.column.ID.String()})
	if err != nil {
		t.Fatalf("encoding the body: %v", err)
	}

	req := httptest.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v1/cards/"+f.alice.card.ID.String()+"/move", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if len(f.events.published()) != 1 {
		t.Fatal("nothing was published for a request whose caller had gone away")
	}

	t.Logf("the publish ran with err=%v, deadline set=%t, budget=%s",
		f.events.ctxErr, f.events.ctxDeadline, f.events.ctxBudget.Round(time.Millisecond))

	if f.events.ctxErr != nil {
		t.Errorf("the publish inherited the request's cancellation (%v); "+
			"one client hanging up would stop the board's other clients being told", f.events.ctxErr)
	}

	if !f.events.ctxDeadline {
		t.Error("the publish context has no deadline; an unreachable broker would hold the request open")
	}

	if f.events.ctxBudget > publishTimeout {
		t.Errorf("the publish budget is %s, over the %s bound", f.events.ctxBudget, publishTimeout)
	}
}

// TestTheResponseWaitsForTheBroadcast is where the ordering guarantee comes
// from.
//
// Publishing in a background goroutine would be cheaper and would break the one
// ordering property a client can actually build on: that two writes it issued
// one after the other — the second sent only after the first was acknowledged —
// arrive in that order at every other client. Doing the publish before the
// response is written is what makes "after the response" mean "after the
// publish". See docs/adr/0005-realtime-event-delivery.md.
func TestTheResponseWaitsForTheBroadcast(t *testing.T) {
	t.Parallel()

	f := newBoardFixture(t)
	f.events.block = make(chan struct{})
	f.events.entered = make(chan struct{})

	done := make(chan int, 1)

	go func() {
		rec := f.do(t, http.MethodPost, "/api/v1/cards/"+f.alice.card.ID.String()+"/move",
			map[string]any{"column_id": f.alice.column.ID.String()}, nil)
		done <- rec.Code
	}()

	select {
	case <-f.events.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the publish was never attempted")
	}

	select {
	case code := <-done:
		t.Fatalf("the response was written (%d) while the broadcast was still in flight; "+
			"two writes from one client could then be published out of order", code)
	case <-time.After(100 * time.Millisecond):
	}

	t.Log("the handler is still inside the publish; the response has not been written")

	close(f.events.block)

	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Errorf("status = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never finished")
	}
}
