package realtime

// The BOLA test for the WebSocket surface: a client authenticated for
// organization A cannot receive organization B's events, whatever it sends.
//
// # Why this file exists separately from auth_bola_test.go
//
// internal/api's BOLA test closed the question for HTTP: no header, query
// parameter, path segment or body field can make a request act in another
// tenant, because the tenant comes from a signed claim. That result carries over
// here unchanged — the upgrade runs behind the same middleware.
//
// What does *not* carry over is the object-level half. A WebSocket client names
// a board id in every subscribe frame, and a board id is exactly the kind of
// identifier that leaks: it appears in URLs, in support tickets, in a restored
// backup. So this surface reintroduces one instance of the problem, over a
// connection that is authorized once and then lives for minutes.
//
// Two independent things stop it, and both are tested with the other one
// neutralised so that neither can be quietly load-bearing on its own:
//
//  1. **The authorizer.** A subscribe is refused unless the subject holds a
//     membership in the tenant *and* the board resolves inside it — which under
//     RLS means the board is the tenant's. TestTheSubscriptionCheckHasTeeth
//     replaces it with the one that only validates the id's format, and shows
//     the refusal turning into an acceptance.
//
//  2. **The room key.** A room is (tenant, board), not (board). Even a
//     subscription that should never have been granted lands in the caller's own
//     tenant's room and cannot be reached by another tenant's publisher.
//     TestTheRoomKeyIsWhatKeepsTenantsApart shows the delivery crossing the
//     moment the tenant is removed from the key.
//
// Two assertions per attack, as in auth_bola_test.go, and the second is the
// stronger one: the response is a refusal, *and* no tenant context was ever
// opened for organization B. A handler that read B's data and then filtered
// would pass the first and fail the second.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

type bolaFixture struct {
	inst  *instance
	store *recordingStore

	tenantA uuid.UUID
	tenantB uuid.UUID

	alice auth.Principal
	bob   auth.Principal

	boardA uuid.UUID
	boardB uuid.UUID

	// cardA and cardB are movable cards on the two boards. The publish half of
	// this test is now a real card move, so each tenant needs something to move.
	cardA board
	cardB board

	aliceToken string
	bobToken   string
}

// newBOLAFixture builds two organizations that share nothing, and one
// authenticated member of each.
//
// The authorizer is the *real* [StoreAuthorizer] over a fake database that
// models the RLS policies: a membership and a board are visible only inside
// their own tenant's transaction. The container-backed version of the same
// scenario, against real policies, is in realtime_integration_test.go.
func newBOLAFixture(t *testing.T, authorizer Authorizer) *bolaFixture {
	t.Helper()

	tenantA := uuid.New()
	tenantB := uuid.New()

	alice := principalFor(tenantA, 15*time.Minute)
	bob := principalFor(tenantB, 15*time.Minute)

	boardA := uuid.New()
	boardB := uuid.New()

	tenantStore := newRecordingStore()
	tenantStore.seedMember(tenantA, alice.UserID)
	tenantStore.seedMember(tenantB, bob.UserID)

	if authorizer == nil {
		authorizer = NewStoreAuthorizer(tenantStore)
	}

	// One store behind both the authorizer and the card routes, so "what alice
	// may subscribe to" and "what alice may move" are answered by the same model
	// of the same policies.
	inst := newInstance(t, instanceOptions{authorizer: authorizer, store: tenantStore})

	return &bolaFixture{
		inst:       inst,
		store:      tenantStore,
		tenantA:    tenantA,
		tenantB:    tenantB,
		alice:      alice,
		bob:        bob,
		boardA:     boardA,
		boardB:     boardB,
		cardA:      inst.seedBoard(tenantA, boardA),
		cardB:      inst.seedBoard(tenantB, boardB),
		aliceToken: inst.token(t, alice),
		bobToken:   inst.token(t, bob),
	}
}

// assertNoForeignTenantOpened checks every tenant context opened since index
// `from` belonged to alice's organization.
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

// TestAClientAuthenticatedForOneTenantCannotReceiveAnothersEvents is the
// headline.
func TestAClientAuthenticatedForOneTenantCannotReceiveAnothersEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	f := newBOLAFixture(t, nil)

	t.Logf("alice is a member of %s and may see board %s; %s and board %s belong to bob",
		f.tenantA, f.boardA, f.tenantB, f.boardB)

	aliceClient := f.inst.dial(ctx, t, f.aliceToken)
	bobClient := f.inst.dial(ctx, t, f.bobToken)

	// The control. Without it every assertion below would also hold for a hub
	// that delivered nothing to anybody.
	t.Run("control: alice watches her own board and receives her own events", func(t *testing.T) {
		aliceClient.subscribe(t, ctx, f.boardA)

		if resp := f.inst.moveCard(t, ctx, f.aliceToken, f.cardA); resp.StatusCode != http.StatusOK {
			t.Fatalf("alice could not move a card on her own board: %d %s", resp.StatusCode, resp.Body)
		}

		frame := aliceClient.expect(FrameEvent, 5*time.Second)
		if frame.BoardID == nil || *frame.BoardID != f.boardA {
			t.Fatalf("alice received %v, want board %s — the fixture is broken and nothing below proves anything",
				frame.BoardID, f.boardA)
		}
	})

	for _, attack := range []struct {
		name    string
		boardID string
		reason  string
	}{
		{
			name:    "subscribing to a board that belongs to the other organization",
			boardID: f.boardB.String(),
			reason:  ReasonForbidden,
		},
		{
			name:    "subscribing to a board that exists nowhere",
			boardID: uuid.NewString(),
			reason:  ReasonForbidden,
		},
		{
			name:    "subscribing to the zero uuid, which is a valid uuid matching nothing",
			boardID: uuid.Nil.String(),
			reason:  ReasonInvalid,
		},
	} {
		t.Run(attack.name, func(t *testing.T) {
			before := len(f.store.openedTenants())

			aliceClient.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: attack.boardID})

			frame := aliceClient.expect(FrameError, 5*time.Second)

			t.Logf("%s -> %s / %s", attack.name, frame.Type, frame.Reason)

			if frame.Reason != attack.reason {
				t.Errorf("reason = %q, want %q", frame.Reason, attack.reason)
			}

			// And the refusal created nothing.
			boardID, err := uuid.Parse(attack.boardID)
			if err == nil {
				for _, room := range []Room{
					{TenantID: f.tenantA, BoardID: boardID},
					{TenantID: f.tenantB, BoardID: boardID},
				} {
					if got := f.inst.hub.connectionsFor(room); got != 0 {
						t.Errorf("a refused subscribe created %d members in %s", got, room)
					}
				}
			}

			f.assertNoForeignTenantOpened(t, before)
		})
	}

	// The most important one: bob really does publish, and alice really does
	// not hear it. A refusal that still left her in the room would pass every
	// assertion above.
	t.Run("bob's events do not reach alice", func(t *testing.T) {
		before := len(f.store.openedTenants())

		bobClient.subscribe(t, ctx, f.boardB)

		if resp := f.inst.moveCard(t, ctx, f.bobToken, f.cardB); resp.StatusCode != http.StatusOK {
			t.Fatalf("bob could not move a card on his own board: %d %s", resp.StatusCode, resp.Body)
		}

		// Bob receives it, so the event was genuinely fanned out.
		bobClient.expect(FrameEvent, 5*time.Second)

		aliceClient.expectNothing(500 * time.Millisecond)

		// Only bob's own request opened bob's tenant. Alice's connection never
		// did.
		opened := f.store.openedTenants()[before:]

		t.Logf("tenant contexts opened while bob moved a card: %v", opened)

		for _, tenantID := range opened {
			if tenantID != f.tenantB {
				t.Errorf("bob's card move opened %s, want only %s", tenantID, f.tenantB)
			}
		}
	})
}

// TestWritingIntoAnotherTenantsBoardPublishesNothing attacks the REST half —
// which since #45 is the card write path rather than a publish endpoint.
//
// The claim is stronger than it looks. It is not only "alice cannot move bob's
// card", which crud_bola_test.go already proves across every endpoint; it is
// that a refused write reaches the fan-out with nothing, so a member of one
// organization cannot make a frame appear in another organization's browser
// under any status code.
func TestWritingIntoAnotherTenantsBoardPublishesNothing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	f := newBOLAFixture(t, nil)

	bobClient := f.inst.dial(ctx, t, f.bobToken)
	bobClient.subscribe(t, ctx, f.boardB)

	// Alice also watches her own board, so an event that was mis-addressed into
	// *her* room instead of refused would be caught rather than pass as silence.
	aliceClient := f.inst.dial(ctx, t, f.aliceToken)
	aliceClient.subscribe(t, ctx, f.boardA)

	for _, attack := range []struct {
		name             string
		cardID, columnID uuid.UUID
	}{
		{
			name:   "bob's card into bob's column: the whole operation transplanted",
			cardID: f.cardB.cardID, columnID: f.cardB.columnID,
		},
		{
			name:   "alice's own card into bob's column: every id real, one of them not hers",
			cardID: f.cardA.cardID, columnID: f.cardB.columnID,
		},
		{
			name:   "a card that exists nowhere",
			cardID: uuid.New(), columnID: f.cardA.columnID,
		},
	} {
		t.Run(attack.name, func(t *testing.T) {
			before := len(f.store.openedTenants())

			resp := f.inst.moveCardByID(t, ctx, f.aliceToken, attack.cardID, attack.columnID)

			t.Logf("%s -> %d %s", attack.name, resp.StatusCode, resp.Body)

			// 404, not 403: answering 403 for a real id and 404 for a fictional
			// one would make this an existence oracle across the tenant
			// boundary. All three attacks answer identically.
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}

			// Nothing reached bob, so the refusal happened before the fan-out
			// and not after it.
			bobClient.expectNothing(300 * time.Millisecond)
			aliceClient.expectNothing(300 * time.Millisecond)

			f.assertNoForeignTenantOpened(t, before)
		})
	}
}

// TestRewritingTheOrgClaimDoesNotOpenAWebSocket is the same attack against the
// only channel that would actually carry a tenant if it were believed.
func TestRewritingTheOrgClaimDoesNotOpenAWebSocket(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	f := newBOLAFixture(t, nil)

	before := len(f.store.openedTenants())

	forged := tamperClaim(t, f.aliceToken, "org", f.tenantB.String())

	_, resp, err := websocketDial(ctx, f.inst.wsURL(), forged)
	if err == nil {
		t.Fatal("a token with a rewritten org claim opened a connection")
	}

	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401 — the signature no longer covers the payload", resp)
	}

	_ = resp.Body.Close()

	f.assertNoForeignTenantOpened(t, before)

	t.Log("the org claim is signed, so rewriting it costs the token its signature")
}

// TestHeadersAndQueryParametersCannotRedirectTheUpgrade re-runs internal/api's
// channel sweep against this route, because a new route is a new chance to add
// a tenant input by accident.
func TestHeadersAndQueryParametersCannotRedirectTheUpgrade(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	f := newBOLAFixture(t, nil)

	for _, attack := range []struct {
		name    string
		headers map[string]string
		query   string
	}{
		{name: "X-Tenant-ID header", headers: map[string]string{"X-Tenant-ID": f.tenantB.String()}},
		{name: "X-Organization-ID header", headers: map[string]string{"X-Organization-ID": f.tenantB.String()}},
		{name: "org query parameter", query: "?org=" + f.tenantB.String()},
		{name: "tenant_id query parameter", query: "?tenant_id=" + f.tenantB.String()},
	} {
		t.Run(attack.name, func(t *testing.T) {
			before := len(f.store.openedTenants())

			header := http.Header{"Authorization": []string{"Bearer " + f.aliceToken}}
			for name, value := range attack.headers {
				header.Set(name, value)
			}

			c := f.inst.dialHeader(ctx, t, f.inst.wsURL()+attack.query, header)

			// The connection opens — the attack channels are ignored, not
			// rejected — and it is still alice's.
			c.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: f.boardB.String()})

			frame := c.expect(FrameError, 5*time.Second)
			if frame.Reason != ReasonForbidden {
				t.Errorf("reason = %q, want %q", frame.Reason, ReasonForbidden)
			}

			c.subscribe(t, ctx, f.boardA)

			f.assertNoForeignTenantOpened(t, before)
		})
	}
}

// formatOnlyAuthorizer is the vulnerability this design is built against: it
// checks that the board id is a well-formed uuid and nothing else.
//
// It is not a straw man. "The room key already contains the tenant, and the
// queries are behind RLS anyway, so what is there to check" is a completely
// reasonable-sounding argument, and it is the argument that produces this.
type formatOnlyAuthorizer struct{}

func (formatOnlyAuthorizer) AuthorizeBoard(_ context.Context, _ auth.Principal, boardID uuid.UUID) error {
	if boardID == uuid.Nil {
		return ErrForbidden
	}

	return nil
}

func (formatOnlyAuthorizer) AuthorizeTenant(context.Context, auth.Principal) error { return nil }

// TestTheSubscriptionCheckHasTeeth shows the refusal above turning into an
// acceptance when the authorization is removed.
//
// A security assertion that has never been observed to fail is a security
// assertion nobody should trust.
func TestTheSubscriptionCheckHasTeeth(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	f := newBOLAFixture(t, formatOnlyAuthorizer{})

	aliceClient := f.inst.dial(ctx, t, f.aliceToken)

	aliceClient.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: f.boardB.String()})

	frame, ok := aliceClient.next(5 * time.Second)
	if !ok {
		t.Fatal("no answer to the subscribe")
	}

	t.Logf("against a format-only authorizer, subscribing to another tenant's board answered %q", frame.Type)

	if frame.Type != FrameSubscribed {
		t.Fatalf("the deliberately vulnerable authorizer refused the subscribe; the assertions in "+
			"TestAClientAuthenticatedForOneTenantCannotReceiveAnothersEvents cannot be trusted to detect one (got %q/%q)",
			frame.Type, frame.Reason)
	}

	if got := f.inst.hub.connectionsFor(Room{TenantID: f.tenantA, BoardID: f.boardB}); got != 1 {
		t.Fatalf("the vulnerable authorizer created %d subscriptions, want 1", got)
	}

	t.Log("confirmed: removing the subscription check lets a member of one organization " +
		"register interest in another organization's board, and the assertions above catch it")

	// And here is the second layer holding anyway. Bob publishes to his own
	// board; alice is registered against the same board id under *her* tenant,
	// so the room keys differ and nothing crosses. This is why the design has
	// two checks rather than one.
	bobClient := f.inst.dial(ctx, t, f.bobToken)
	bobClient.subscribe(t, ctx, f.boardB)

	f.inst.moveCard(t, ctx, f.bobToken, f.cardB)
	bobClient.expect(FrameEvent, 5*time.Second)

	aliceClient.expectNothing(500 * time.Millisecond)

	t.Logf("the room key kept them apart: %q vs %q",
		Room{TenantID: f.tenantA, BoardID: f.boardB}.Channel(),
		Room{TenantID: f.tenantB, BoardID: f.boardB}.Channel())
}

// TestTheRoomKeyIsWhatKeepsTenantsApart is the teeth test for the *second*
// layer.
//
// The previous test relies on the room key to contain the damage. This one
// removes that containment — by publishing to the room key a board-only
// implementation would have produced — and shows the delivery crossing. Without
// it, "alice heard nothing" could be true for a reason nobody had identified.
func TestTheRoomKeyIsWhatKeepsTenantsApart(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	f := newBOLAFixture(t, formatOnlyAuthorizer{})

	aliceClient := f.inst.dial(ctx, t, f.aliceToken)

	// Alice registers interest in bob's board. With the real authorizer this is
	// impossible; the previous test is what proves that.
	aliceClient.subscribe(t, ctx, f.boardB)

	// A publisher that keyed the room on the board alone would have addressed
	// the event to whichever tenant's key it happened to compute — here,
	// alice's. That is the mistake being modelled.
	if err := f.inst.hub.Publish(ctx, Room{TenantID: f.tenantA, BoardID: f.boardB}, Event{
		ID:         uuid.New(),
		Type:       "card.moved",
		ActorID:    f.bob.UserID,
		OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	frame := aliceClient.expect(FrameEvent, 5*time.Second)

	t.Logf("with the tenant taken out of the room key, an event for board %s reached a member of %s: actor %s",
		f.boardB, f.tenantA, frame.Event.ActorID)

	t.Log("confirmed: the tenant half of the room key is load-bearing, and expectNothing does detect a crossing")
}

// leakyAuthorizer models the other plausible mistake: a board lookup helper
// that takes a board id and finds the tenant that owns it, rather than one that
// looks inside the caller's tenant.
//
// It is the reason the second assertion — "no tenant context was ever opened
// for organization B" — exists at all: this implementation would answer
// "forbidden" correctly and still have read another organization's data to
// decide.
type leakyAuthorizer struct {
	store    *recordingStore
	tenants  []uuid.UUID
	delegate Authorizer
}

func (a *leakyAuthorizer) AuthorizeBoard(ctx context.Context, principal auth.Principal, boardID uuid.UUID) error {
	// "Find the board first, then check it belongs to the caller." Reads every
	// tenant to answer a question about one.
	for _, tenantID := range a.tenants {
		_ = a.store.WithTenant(ctx, tenantID, func(context.Context, store.Querier) error { return nil })
	}

	return a.delegate.AuthorizeBoard(ctx, principal, boardID)
}

func (a *leakyAuthorizer) AuthorizeTenant(ctx context.Context, principal auth.Principal) error {
	return a.delegate.AuthorizeTenant(ctx, principal)
}

// TestTheOpenedTenantAssertionHasTeeth shows that recording tenant contexts
// catches a leak the response body would hide.
func TestTheOpenedTenantAssertionHasTeeth(t *testing.T) {
	t.Parallel()

	tenantA := uuid.New()
	tenantB := uuid.New()

	alice := principalFor(tenantA, 15*time.Minute)
	boardB := uuid.New()

	tenantStore := newRecordingStore()
	tenantStore.seedMember(tenantA, alice.UserID)
	tenantStore.seedBoard(tenantB, boardB)

	authorizer := &leakyAuthorizer{
		store:    tenantStore,
		tenants:  []uuid.UUID{tenantA, tenantB},
		delegate: NewStoreAuthorizer(tenantStore),
	}

	// The answer is still "forbidden" — which is why the response alone proves
	// nothing.
	if err := authorizer.AuthorizeBoard(t.Context(), alice, boardB); err == nil {
		t.Fatal("the leaky authorizer allowed the subscription; it was supposed to refuse and leak anyway")
	}

	opened := tenantStore.openedTenants()

	t.Logf("the leaky authorizer refused the subscription and opened tenant contexts: %v", opened)

	if !slices.Contains(opened, tenantB) {
		t.Fatal("the leaky authorizer did not open a foreign tenant context; " +
			"assertNoForeignTenantOpened cannot detect one either")
	}

	t.Log("confirmed: a refusal reached by reading another organization's data is detectable, " +
		"and the second assertion is what detects it")
}

// tamperClaim rewrites one claim and leaves the signature untouched.
func tamperClaim(t *testing.T, token, claim, value string) string {
	t.Helper()

	parts := bytes.Split([]byte(token), []byte("."))
	if len(parts) != 3 {
		t.Fatalf("token is not a jwt: %q", token)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		t.Fatalf("decoding the payload: %v", err)
	}

	payload[claim] = value

	edited, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encoding the payload: %v", err)
	}

	return string(parts[0]) + "." + base64.RawURLEncoding.EncodeToString(edited) + "." + string(parts[2])
}
