//go:build integration

package realtime

// The claims that only a real Postgres and a real Redis can settle.
//
//  1. An event published on one API instance reaches a client connected to a
//     different one, with Redis relaying. Single-instance fan-out proves
//     nothing about this: it works identically whether the broker is a network
//     or a channel.
//  2. Subscription authorization holds against the *actual* row-level security
//     policies, not against a fake that models them. The fake could be wrong.
//  3. A membership revoked in the database closes the live connections it
//     authorized.
//  4. End-to-end latency against the vault's ~200 ms target, measured across
//     two instances so the number includes the Redis hop.
//  5. That a card move which does not commit broadcasts nothing, and that two
//     sequential moves of one card arrive in the order they were made (#45).
//
// Since #45 nothing here publishes directly: every event below exists because a
// card or column write committed against the real database, through the real
// endpoint. The demo publish endpoint this suite used to drive is gone.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
	"github.com/AndyV99/collabboard/apps/api/internal/testsupport/pgtest"
)

// redisDB keeps this suite's keys away from anything else in the container.
// Pub/sub itself is not namespaced by database in Redis — PUBLISH crosses them
// — which is fine here because every channel name contains freshly generated
// uuids.
const redisDB = 3

// newRedisInstance builds one API process backed by the real Redis and the real
// Postgres: a RedisBroker, a StoreAuthorizer over an app-role pool, the real
// router, and an HTTP server.
func newRedisInstance(t *testing.T, issuer *auth.Issuer, reauthorizeInterval time.Duration) *instance {
	t.Helper()

	gin.SetMode(gin.TestMode)

	broker, err := NewRedisBroker(RedisBrokerConfig{
		// A client per instance, as a separate process would have.
		Client: testRedis.Client(t, redisDB),
		Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("building the redis broker: %v", err)
	}

	// One pool as collabboard_app — the only identity the policies apply to —
	// behind both the subscription authorizer and the card endpoints, so this
	// instance authorizes a subscribe and executes a move against exactly the
	// same policies a deployed one would.
	appStore := testDB.AppStore(t, 4)

	hub, err := NewHub(HubConfig{
		Broker:              broker,
		Authorizer:          NewStoreAuthorizer(appStore),
		Logger:              discardLogger(),
		ReauthorizeInterval: reauthorizeInterval,
		AllowedOrigins:      []string{"*"},
	})
	if err != nil {
		t.Fatalf("building the hub: %v", err)
	}

	// The real router with the real board routes. Since #45 the only thing that
	// can publish is a card or column write that committed, so every assertion
	// below about fan-out is also an assertion about the write path.
	router := api.NewRouter(discardLogger(),
		api.HealthDeps{},
		api.AuthDeps{Service: stubAuthService{}, Verifier: issuer, Store: appStore},
		api.RealtimeDeps{Connect: hub.ConnectHandler(), Publisher: hub.EventPublisher()})

	server := httptest.NewServer(router)

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := hub.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutting the hub down: %v", err)
		}

		server.Close()
	})

	return &instance{t: t, hub: hub, server: server, issuer: issuer}
}

// memberPrincipal builds a principal for a seeded member of a seeded tenant.
func memberPrincipal(tenant pgtest.Tenant) auth.Principal {
	return auth.Principal{
		UserID:    tenant.MemberID,
		TenantID:  tenant.TenantID,
		Role:      "owner",
		SessionID: uuid.New(),
	}
}

// moveSeededCard moves the fixture's card to the front of its own column,
// through the real endpoint, against the real database.
//
// It is a no-op as far as the board's order goes — one card, one column — which
// is exactly what makes it a clean signal: the row is written, the transaction
// commits, and the only observable consequence is the event.
func moveSeededCard(t *testing.T, ctx context.Context, inst *instance, token string, tenant pgtest.Tenant) httpResult {
	t.Helper()

	return inst.moveCardByID(t, ctx, token, tenant.CardID, tenant.ColumnID)
}

// TestAnEventCrossesTwoInstancesThroughRedis is the acceptance criterion the
// issue says is the one most likely to be quietly skipped.
func TestAnEventCrossesTwoInstancesThroughRedis(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	first := newRedisInstance(t, issuer, time.Minute)
	second := newRedisInstance(t, issuer, time.Minute)

	tenant := fixture.A

	// The publisher is the tenant's own member; the watcher is the user who
	// belongs to both organizations, connected on the *other* instance.
	publisher := memberPrincipal(tenant)
	watcher := auth.Principal{
		UserID:    fixture.SharedUserID,
		TenantID:  tenant.TenantID,
		Role:      "member",
		SessionID: uuid.New(),
	}

	watcherClient := second.dial(ctx, t, second.token(t, watcher))
	watcherClient.subscribe(t, ctx, tenant.BoardID)

	// Nothing on the publishing instance is watching this board, so a delivery
	// there could not explain the result.
	if rooms, conns := first.hub.Rooms(); rooms != 0 || conns != 0 {
		t.Fatalf("the publishing instance holds %d rooms / %d connections; it should hold none", rooms, conns)
	}

	if got := second.hub.connectionsFor(Room{TenantID: tenant.TenantID, BoardID: tenant.BoardID}); got != 1 {
		t.Fatalf("the watching instance holds %d connections for the board, want 1", got)
	}

	started := time.Now()

	resp := moveSeededCard(t, ctx, first, first.token(t, publisher), tenant)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move status = %d, want 200: %s", resp.StatusCode, resp.Body)
	}

	frame := watcherClient.expect(FrameEvent, 10*time.Second)

	t.Logf("an event published on instance one reached a client on instance two in %s, relayed by Redis",
		time.Since(started))

	if frame.Event.ActorID != publisher.UserID {
		t.Errorf("actor = %s, want %s", frame.Event.ActorID, publisher.UserID)
	}

	if frame.BoardID == nil || *frame.BoardID != tenant.BoardID {
		t.Errorf("board = %v, want %s", frame.BoardID, tenant.BoardID)
	}
}

// TestCrossInstanceLatency measures the number the vault's ~200 ms target is
// about, across two instances so it includes the Redis hop.
func TestCrossInstanceLatency(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	const samples = 50

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	first := newRedisInstance(t, issuer, time.Minute)
	second := newRedisInstance(t, issuer, time.Minute)

	tenant := fixture.A
	publisher := memberPrincipal(tenant)
	token := first.token(t, publisher)

	watcher := second.dial(ctx, t, second.token(t, memberPrincipal(tenant)))
	watcher.subscribe(t, ctx, tenant.BoardID)

	latencies := make([]time.Duration, 0, samples)

	for range samples {
		started := time.Now()

		if resp := moveSeededCard(t, ctx, first, token, tenant); resp.StatusCode != http.StatusOK {
			t.Fatalf("move status = %d, want 200: %s", resp.StatusCode, resp.Body)
		}

		watcher.expect(FrameEvent, 10*time.Second)

		latencies = append(latencies, time.Since(started))
	}

	slices.Sort(latencies)

	p := func(q float64) time.Duration {
		index := int(q * float64(len(latencies)-1))

		return latencies[index]
	}

	// The measurement covers the whole path a card move takes: an authenticated
	// HTTP request on instance one, a board authorization against Postgres, a
	// Redis PUBLISH, instance two's subscription, its fan-out, and the
	// WebSocket frame arriving at the client. It is not a Redis benchmark.
	t.Logf("cross-instance end-to-end latency over %d samples: p50=%s p95=%s p99=%s max=%s (target ~200ms)",
		samples, p(0.50), p(0.95), p(0.99), latencies[len(latencies)-1])

	const target = 200 * time.Millisecond

	if p(0.95) > target {
		t.Errorf("p95 latency %s exceeds the ~%s target", p(0.95), target)
	}
}

// TestSubscriptionAuthorizationHoldsAgainstRealPolicies is the cross-tenant
// test from bola_test.go, re-run against the database rather than a model of
// it.
func TestSubscriptionAuthorizationHoldsAgainstRealPolicies(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	inst := newRedisInstance(t, issuer, time.Minute)

	alice := memberPrincipal(fixture.A)
	bob := memberPrincipal(fixture.B)

	aliceToken := inst.token(t, alice)
	bobToken := inst.token(t, bob)

	aliceClient := inst.dial(ctx, t, aliceToken)
	bobClient := inst.dial(ctx, t, bobToken)

	// The control, against real RLS: alice can watch her own board.
	aliceClient.subscribe(t, ctx, fixture.A.BoardID)

	// The attack: alice names bob's board id, which really exists.
	aliceClient.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: fixture.B.BoardID.String()})

	frame := aliceClient.expect(FrameError, 10*time.Second)

	t.Logf("alice subscribing to %s's board -> %s / %s", fixture.B.Label, frame.Type, frame.Reason)

	if frame.Reason != ReasonForbidden {
		t.Fatalf("reason = %q, want %q", frame.Reason, ReasonForbidden)
	}

	if got := inst.hub.connectionsFor(Room{TenantID: fixture.A.TenantID, BoardID: fixture.B.BoardID}); got != 0 {
		t.Errorf("a refused subscribe created %d members", got)
	}

	// And writing into it is refused the same way — 404 rather than 403, because
	// under RLS bob's card does not exist to alice's transaction and answering
	// 403 would confirm that it does.
	if resp := moveSeededCard(t, ctx, inst, aliceToken, fixture.B); resp.StatusCode != http.StatusNotFound {
		t.Errorf("alice moving a card on bob's board -> %d, want 404: %s", resp.StatusCode, resp.Body)
	}

	// Now bob really does move a card, and alice really does not hear it.
	bobClient.subscribe(t, ctx, fixture.B.BoardID)

	if resp := moveSeededCard(t, ctx, inst, bobToken, fixture.B); resp.StatusCode != http.StatusOK {
		t.Fatalf("bob could not move a card on his own board: %d %s", resp.StatusCode, resp.Body)
	}

	bobClient.expect(FrameEvent, 10*time.Second)
	aliceClient.expectNothing(time.Second)

	t.Log("against live row-level security, a board id from another organization is refused and delivers nothing")
}

// TestRevokingAMembershipInTheDatabaseClosesLiveConnections is the answer to
// "what happens when a membership is revoked while a socket is open".
func TestRevokingAMembershipInTheDatabaseClosesLiveConnections(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	// A short sweep so the test does not wait 30 seconds. The interval is a
	// configuration value precisely so that this bound is a decision rather
	// than a constant somebody has to rediscover.
	inst := newRedisInstance(t, issuer, 200*time.Millisecond)

	tenant := fixture.A

	// The shared user, so revoking their membership in A leaves them a member
	// of B — which is what makes this a revocation rather than a deletion.
	principal := auth.Principal{
		UserID:    fixture.SharedUserID,
		TenantID:  tenant.TenantID,
		Role:      "member",
		SessionID: uuid.New(),
	}

	c := inst.dial(ctx, t, inst.token(t, principal))
	c.subscribe(t, ctx, tenant.BoardID)

	started := time.Now()

	// As the owner, because the policies deliberately stop a tenant-scoped
	// connection from doing this to itself.
	testDB.OwnerExec(t, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`,
		tenant.TenantID, fixture.SharedUserID)

	if got := c.closeStatus(20 * time.Second); got != StatusMembershipRevoked {
		t.Fatalf("close status = %d, want %d (membership revoked)", got, StatusMembershipRevoked)
	}

	t.Logf("a membership revoked in the database closed the live connection %s later; "+
		"the access token that opened it is still valid and still useless",
		time.Since(started).Round(time.Millisecond))

	eventually(t, 10*time.Second, "the revoked connection to be deregistered", func() bool {
		rooms, conns := inst.hub.Rooms()

		return rooms == 0 && conns == 0
	})

	// And a fresh connection with the same still-valid token cannot subscribe.
	fresh := inst.dial(ctx, t, inst.token(t, principal))
	fresh.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: tenant.BoardID.String()})

	frame := fresh.expect(FrameError, 10*time.Second)
	if frame.Reason != ReasonForbidden {
		t.Errorf("reason = %q, want %q", frame.Reason, ReasonForbidden)
	}
}

// TestARestartingInstanceHandsItsClientsOver is the instance-restart story,
// end to end over Redis.
//
// The clients of the instance going down are told to reconnect, with a
// jittered delay, and the surviving instance keeps serving throughout. Nothing
// is replayed: an event published during the gap is gone, which is why a client
// re-fetches the board when it re-subscribes.
func TestARestartingInstanceHandsItsClientsOver(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	restarting := newRedisInstance(t, issuer, time.Minute)
	surviving := newRedisInstance(t, issuer, time.Minute)

	tenant := fixture.A
	principal := memberPrincipal(tenant)
	token := restarting.token(t, principal)

	moving := restarting.dial(ctx, t, token)
	moving.subscribe(t, ctx, tenant.BoardID)

	staying := surviving.dial(ctx, t, surviving.token(t, principal))
	staying.subscribe(t, ctx, tenant.BoardID)

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 20*time.Second)
	defer shutdownCancel()

	if err := restarting.hub.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("draining the restarting instance: %v", err)
	}

	frame := moving.expect(FrameShutdown, 10*time.Second)

	t.Logf("the draining instance told its client to reconnect after %dms", frame.ReconnectAfterMs)

	if frame.ReconnectAfterMs <= 0 {
		t.Error("no reconnect delay; every client of a restarting instance would reconnect at once")
	}

	if got := moving.closeStatus(10 * time.Second); got != closeGoingAway {
		t.Errorf("close status = %d, want %d (going away)", got, closeGoingAway)
	}

	// The surviving instance is unaffected, including its Redis subscription.
	if resp := moveSeededCard(t, ctx, surviving, surviving.token(t, principal),
		tenant); resp.StatusCode != http.StatusOK {
		t.Fatalf("moving a card on the surviving instance: %d %s", resp.StatusCode, resp.Body)
	}

	staying.expect(FrameEvent, 10*time.Second)

	// And the client that was moved reconnects to the survivor and carries on.
	reconnected := surviving.dial(ctx, t, surviving.token(t, principal))
	reconnected.subscribe(t, ctx, tenant.BoardID)

	if resp := moveSeededCard(t, ctx, surviving, surviving.token(t, principal),
		tenant); resp.StatusCode != http.StatusOK {
		t.Fatalf("moving a card after the handover: %d %s", resp.StatusCode, resp.Body)
	}

	reconnected.expect(FrameEvent, 10*time.Second)

	t.Log("the drained client reconnected to the other instance and resumed receiving events")
}

// TestASubscriptionIsLiveBeforeItIsAcknowledged is the reason RedisBroker's
// Subscribe waits for Redis to confirm.
//
// Without the confirmation there is a window between "the SUBSCRIBE was
// written" and "Redis processed it", and a publish landing in that window is
// lost. It is small, load-dependent, and therefore exactly the kind of bug that
// shows up once a quarter in production and never in a test. This asserts the
// property the confirmation buys: publish immediately after the acknowledgement
// and it always arrives.
func TestASubscriptionIsLiveBeforeItIsAcknowledged(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	const rounds = 25

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	first := newRedisInstance(t, issuer, time.Minute)
	second := newRedisInstance(t, issuer, time.Minute)

	tenant := fixture.A
	principal := memberPrincipal(tenant)
	token := first.token(t, principal)

	for round := range rounds {
		c := second.dial(ctx, t, second.token(t, principal))

		// Returns only once the acknowledgement has arrived, which the server
		// only sends after Redis confirmed the SUBSCRIBE.
		c.subscribe(t, ctx, tenant.BoardID)

		if resp := moveSeededCard(t, ctx, first, token, tenant); resp.StatusCode != http.StatusOK {
			t.Fatalf("round %d: move status = %d %s", round, resp.StatusCode, resp.Body)
		}

		c.expect(FrameEvent, 10*time.Second)

		if err := c.ws.Close(closeNormal, "round over"); err != nil {
			t.Fatalf("round %d: closing: %v", round, err)
		}
	}

	t.Logf("%d subscribe-then-publish rounds across two instances, no lost first event", rounds)
}

// TestAMoveThatDoesNotCommitBroadcastsNothing is #45's correctness claim,
// against a real database.
//
// Two halves, because the claim has two.
//
// The first shows Postgres really does undo a real write when the transaction
// fails: a card is renamed inside a tenant transaction that then returns an
// error, and the card comes back with its old title. Without that half,
// "nothing was broadcast" could be true because nothing ever happened.
//
// The second shows the handler's side: a move the database refuses answers 409
// and the client watching that board hears nothing at all. A publish from inside
// the transaction would have gone out before the refusal was known.
func TestAMoveThatDoesNotCommitBroadcastsNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	inst := newRedisInstance(t, issuer, time.Minute)

	tenant := fixture.A
	principal := memberPrincipal(tenant)
	token := inst.token(t, principal)

	watcher := inst.dial(ctx, t, token)
	watcher.subscribe(t, ctx, tenant.BoardID)

	// Half one: a real write, really rolled back.
	appStore := testDB.AppStore(t, 2)
	doomed := errors.New("the caller changed its mind after writing")
	renamed := "renamed by a transaction that did not commit"

	err := appStore.WithTenant(ctx, tenant.TenantID, func(ctx context.Context, q store.Querier) error {
		if _, err := q.UpdateCard(ctx, store.UpdateCardParams{CardID: tenant.CardID, Title: &renamed}); err != nil {
			return err
		}

		return doomed
	})
	if !errors.Is(err, doomed) {
		t.Fatalf("the transaction was supposed to fail; got %v", err)
	}

	card := inst.request(t, ctx, http.MethodGet, token, "/api/v1/cards/"+tenant.CardID.String(), "")
	if card.StatusCode != http.StatusOK {
		t.Fatalf("reading the card back: %d %s", card.StatusCode, card.Body)
	}

	if strings.Contains(card.Body, renamed) {
		t.Fatalf("the rolled-back rename survived: %s", card.Body)
	}

	t.Logf("a real write was rolled back and the card still reads %q", tenant.CardTitle)

	// Half two: a move the database refuses. The anchor names a card in another
	// organization, which the policy has already made invisible, so MoveCard
	// matches no row and the whole transaction unwinds.
	refused := inst.request(t, ctx, http.MethodPost, token,
		"/api/v1/cards/"+tenant.CardID.String()+"/move",
		`{"column_id":"`+tenant.ColumnID.String()+`","after_card_id":"`+fixture.B.CardID.String()+`"}`)

	t.Logf("a move with an anchor the database will not accept -> %d %s", refused.StatusCode, refused.Body)

	if refused.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", refused.StatusCode)
	}

	watcher.expectNothing(2 * time.Second)

	t.Log("no frame reached the board's subscriber: the publish is downstream of the commit, " +
		"so a transaction that did not commit cannot announce anything")

	// The control. The same client, the same board, a move that *does* commit —
	// so the silence above is about the refusal and not about the fixture.
	if resp := moveSeededCard(t, ctx, inst, token, tenant); resp.StatusCode != http.StatusOK {
		t.Fatalf("the control move failed: %d %s", resp.StatusCode, resp.Body)
	}

	frame := watcher.expect(FrameEvent, 10*time.Second)

	t.Logf("and a move that did commit was delivered: %s", frame.Event.Type)
}

// movedColumn is the part of a card.moved payload this suite reads back.
type movedColumn struct {
	Card struct {
		ID       string `json:"id"`
		ColumnID string `json:"column_id"`
	} `json:"card"`
}

// TestTwoRapidMovesOfTheSameCardArriveInOrder is the ordering guarantee, stated
// as the thing a client can actually build on.
//
// The guarantee is causal rather than wall-clock: a client that issues its
// second move only after the first one's response has arrived is guaranteed that
// every other client sees them in that order, because the publish happens before
// the response is written and Redis delivers one total order per channel. That
// is the case a user produces by dragging a card twice, and it is the case the
// design has to get right — see docs/adr/0005-realtime-event-delivery.md, which
// is equally explicit that two *concurrent* movers get an order the server
// picked rather than a promise about which of them wins.
func TestTwoRapidMovesOfTheSameCardArriveInOrder(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	const rounds = 20

	fixture := testDB.Seed(t)
	issuer := testIssuer(t, 15*time.Minute)

	// Two instances, so the order under test is the order Redis chose and not
	// one process's view of its own work.
	mover := newRedisInstance(t, issuer, time.Minute)
	observer := newRedisInstance(t, issuer, time.Minute)

	tenant := fixture.A
	principal := memberPrincipal(tenant)
	token := mover.token(t, principal)

	watcher := observer.dial(ctx, t, observer.token(t, principal))
	watcher.subscribe(t, ctx, tenant.BoardID)

	// A second column to move between, created through the API — which also
	// shows column.created reaching the same room.
	created := mover.request(t, ctx, http.MethodPost, token,
		"/api/v1/boards/"+tenant.BoardID.String()+"/columns", `{"name":"Doing"}`)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("creating a second column: %d %s", created.StatusCode, created.Body)
	}

	var columnResponse struct {
		Column struct {
			ID string `json:"id"`
		} `json:"column"`
	}

	if err := json.Unmarshal([]byte(created.Body), &columnResponse); err != nil {
		t.Fatalf("decoding the column: %v", err)
	}

	columns := [2]uuid.UUID{tenant.ColumnID, uuid.MustParse(columnResponse.Column.ID)}

	if frame := watcher.expect(FrameEvent, 10*time.Second); frame.Event.Type != "column.created" {
		t.Fatalf("first event = %q, want column.created", frame.Event.Type)
	}

	// Each move is issued only after the previous one's response, which is
	// exactly the "two rapid moves" a dragging user produces.
	want := make([]uuid.UUID, 0, rounds)

	for round := range rounds {
		target := columns[(round+1)%2]

		if resp := mover.moveCardByID(t, ctx, token, tenant.CardID, target); resp.StatusCode != http.StatusOK {
			t.Fatalf("round %d: %d %s", round, resp.StatusCode, resp.Body)
		}

		want = append(want, target)
	}

	got := make([]uuid.UUID, 0, rounds)

	for range rounds {
		frame := watcher.expect(FrameEvent, 10*time.Second)

		if frame.Event.Type != "card.moved" {
			t.Fatalf("unexpected event %q while collecting moves", frame.Event.Type)
		}

		var payload movedColumn
		if err := json.Unmarshal(frame.Event.Payload, &payload); err != nil {
			t.Fatalf("decoding a card.moved payload: %v", err)
		}

		if payload.Card.ID != tenant.CardID.String() {
			t.Fatalf("an event named card %s, want %s", payload.Card.ID, tenant.CardID)
		}

		got = append(got, uuid.MustParse(payload.Card.ColumnID))
	}

	t.Logf("%d sequential moves of one card, observed on another instance", rounds)

	if !slices.Equal(got, want) {
		t.Fatalf("the moves arrived out of order.\nwant %v\ngot  %v", want, got)
	}

	// And the last event agrees with the database, which is the failure this
	// guarantee exists to prevent: a client whose board disagrees with the
	// server.
	final := mover.request(t, ctx, http.MethodGet, token, "/api/v1/cards/"+tenant.CardID.String(), "")
	if !strings.Contains(final.Body, got[len(got)-1].String()) {
		t.Errorf("the last event put the card in %s; the database says %s", got[len(got)-1], final.Body)
	}

	t.Log("the last event and the database agree on which column the card is in")
}
