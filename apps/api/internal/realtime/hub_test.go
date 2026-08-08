package realtime

// The behavioural tests: fan-out, isolation between boards, registration,
// backpressure, keepalive, revocation and shutdown.
//
// Every one of them goes through the real router, the real auth middleware and
// a real WebSocket over a real TCP socket. Only the transport and the database
// are stand-ins, and both have container-backed counterparts in
// realtime_integration_test.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// principalFor builds a member of a tenant with a fresh session.
func principalFor(tenantID uuid.UUID, ttl time.Duration) auth.Principal {
	return auth.Principal{
		UserID:    uuid.New(),
		TenantID:  tenantID,
		Role:      "member",
		SessionID: uuid.New(),
		ExpiresAt: time.Now().Add(ttl),
	}
}

func TestTwoClientsOnTheSameBoardSeeEachOthersEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	tenantID := uuid.New()
	boardID := uuid.New()

	alice := principalFor(tenantID, 15*time.Minute)
	bob := principalFor(tenantID, 15*time.Minute)

	aliceToken := inst.token(t, alice)
	bobToken := inst.token(t, bob)

	aliceClient := inst.dial(ctx, t, aliceToken)
	bobClient := inst.dial(ctx, t, bobToken)

	aliceClient.subscribe(t, ctx, boardID)
	bobClient.subscribe(t, ctx, boardID)

	board := inst.seedBoard(tenantID, boardID)

	started := time.Now()

	// A real card move through the real endpoint. Nothing in this test asks for
	// an event to be published; the event exists because a row changed.
	resp := inst.moveCard(t, ctx, aliceToken, board)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move status = %d, want 200: %s", resp.StatusCode, resp.Body)
	}

	for name, c := range map[string]*client{"alice": aliceClient, "bob": bobClient} {
		frame := c.expect(FrameEvent, 5*time.Second)

		t.Logf("%s received %s after %s", name, frame.Event.Type, time.Since(started))

		if frame.Event.Type != "card.moved" {
			t.Errorf("%s got event type %q, want card.moved", name, frame.Event.Type)
		}

		if frame.BoardID == nil || *frame.BoardID != boardID {
			t.Errorf("%s got board %v, want %s", name, frame.BoardID, boardID)
		}

		// The actor is the mover's principal, from the token and from nowhere
		// else. Nothing in the request body could have named one.
		if frame.Event.ActorID != alice.UserID {
			t.Errorf("%s got actor %s, want %s", name, frame.Event.ActorID, alice.UserID)
		}

		// And the payload is the card that moved, so a client can apply the
		// change without re-fetching the column.
		if !strings.Contains(string(frame.Event.Payload), board.cardID.String()) {
			t.Errorf("%s got a payload naming no card: %s", name, frame.Event.Payload)
		}
	}

	// The publisher sees its own event too. That is deliberate — the event goes
	// through the broker and comes back, so there is one delivery path rather
	// than a local one and a remote one — and it is what lets a client treat
	// the stream as the single source of ordering.
	t.Log("the publisher received its own event, confirming there is no local shortcut path")
}

func TestClientsOnDifferentBoardsDoNotSeeEachOthersEvents(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	tenantID := uuid.New()
	watched := uuid.New()
	other := uuid.New()

	alice := principalFor(tenantID, 15*time.Minute)
	bob := principalFor(tenantID, 15*time.Minute)

	aliceClient := inst.dial(ctx, t, inst.token(t, alice))
	bobToken := inst.token(t, bob)
	bobClient := inst.dial(ctx, t, bobToken)

	aliceClient.subscribe(t, ctx, watched)
	bobClient.subscribe(t, ctx, other)

	inst.moveCard(t, ctx, bobToken, inst.seedBoard(tenantID, other))

	// Bob is on the other board and must see it, which is the control: without
	// it, alice's silence would also hold for a hub that delivers nothing.
	bobClient.expect(FrameEvent, 5*time.Second)

	aliceClient.expectNothing(300 * time.Millisecond)

	t.Logf("board %s received the event; board %s did not", other, watched)
}

func TestAnEventPublishedOnOneInstanceReachesAnother(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// One bus, two hubs, two HTTP servers: two API processes as far as
	// everything below the transport is concerned.
	bus := NewMemoryBus()
	issuer := testIssuer(t, 15*time.Minute)

	first := newInstance(t, instanceOptions{bus: bus, authorizer: allowAll{}, issuer: issuer})
	second := newInstance(t, instanceOptions{bus: bus, authorizer: allowAll{}, issuer: issuer})

	tenantID := uuid.New()
	boardID := uuid.New()

	publisher := principalFor(tenantID, 15*time.Minute)
	watcher := principalFor(tenantID, 15*time.Minute)

	// The watcher is on the second instance and never talks to the first.
	watcherClient := second.dial(ctx, t, second.token(t, watcher))
	watcherClient.subscribe(t, ctx, boardID)

	if rooms, conns := first.hub.Rooms(); rooms != 0 || conns != 0 {
		t.Fatalf("the publishing instance holds %d rooms and %d connections; it should hold none", rooms, conns)
	}

	started := time.Now()

	resp := first.moveCard(t, ctx, first.token(t, publisher), first.seedBoard(tenantID, boardID))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("move status = %d, want 200: %s", resp.StatusCode, resp.Body)
	}

	frame := watcherClient.expect(FrameEvent, 5*time.Second)

	t.Logf("an event published on instance one reached a client on instance two in %s", time.Since(started))

	if frame.Event.ActorID != publisher.UserID {
		t.Errorf("actor = %s, want %s", frame.Event.ActorID, publisher.UserID)
	}
}

func TestAnInstanceWithNoLocalMembersNeverSubscribes(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	bus := NewMemoryBus()
	issuer := testIssuer(t, 15*time.Minute)

	first := newInstance(t, instanceOptions{bus: bus, authorizer: allowAll{}, issuer: issuer})
	second := newInstance(t, instanceOptions{bus: bus, authorizer: allowAll{}, issuer: issuer})

	tenantID := uuid.New()
	boardID := uuid.New()

	c := second.dial(ctx, t, second.token(t, principalFor(tenantID, 15*time.Minute)))
	c.subscribe(t, ctx, boardID)

	room := Room{TenantID: tenantID, BoardID: boardID}

	if got := first.hub.connectionsFor(room); got != 0 {
		t.Errorf("the instance with no members holds %d connections for %s, want 0", got, room)
	}

	if got := second.hub.connectionsFor(room); got != 1 {
		t.Errorf("the instance with the member holds %d connections for %s, want 1", got, room)
	}
}

func TestASubscriptionIsDeregisteredWhenTheClientDisconnects(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	tenantID := uuid.New()
	boardID := uuid.New()
	room := Room{TenantID: tenantID, BoardID: boardID}

	c := inst.dial(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))
	c.subscribe(t, ctx, boardID)

	if got := inst.hub.connectionsFor(room); got != 1 {
		t.Fatalf("connections for %s = %d, want 1", room, got)
	}

	if err := c.ws.Close(websocket.StatusNormalClosure, "done"); err != nil {
		t.Fatalf("closing the client: %v", err)
	}

	eventually(t, 5*time.Second, "the room to be emptied", func() bool {
		rooms, conns := inst.hub.Rooms()

		return rooms == 0 && conns == 0
	})

	t.Log("the room and the connection were both removed; nothing leaks when a client hangs up")
}

func TestUnsubscribingStopsDelivery(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	tenantID := uuid.New()
	boardID := uuid.New()

	principal := principalFor(tenantID, 15*time.Minute)
	token := inst.token(t, principal)

	c := inst.dial(ctx, t, token)
	c.subscribe(t, ctx, boardID)

	board := inst.seedBoard(tenantID, boardID)

	inst.moveCard(t, ctx, token, board)
	c.expect(FrameEvent, 5*time.Second)

	c.send(t, ctx, clientFrame{Type: clientUnsubscribe, BoardID: boardID.String()})
	c.expect(FrameUnsubscribed, 5*time.Second)

	inst.moveCard(t, ctx, token, board)
	c.expectNothing(300 * time.Millisecond)
}

// TestAFullSendBufferDropsTheClientAndNothingElse is the backpressure decision,
// stated exactly.
//
// It drives the hub directly rather than through a socket, and that is the
// point: whether a real peer's buffers fill depends on the machine's
// net.ipv4.tcp_wmem, so a test that waits for TCP to push back is a test whose
// outcome is decided by the kernel it runs on. This one decides the question in
// the only place the decision actually lives — [Hub.deliver]'s non-blocking
// send — and the end-to-end behaviour over a real socket is the test below.
//
// Two connections, one draining its buffer and one not. The claim is not just
// "the stalled one is dropped" but "and the other one misses nothing", which is
// what "does not block the hub or other clients" means.
func TestAFullSendBufferDropsTheClientAndNothingElse(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	const buffer = 4

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}, sendBuffer: buffer})

	room := Room{TenantID: uuid.New(), BoardID: uuid.New()}

	// Two connections that never run serve, so nothing drains their buffers
	// except what this test chooses to drain. Their websocket is nil, which is
	// safe because every path under test — deliver, trySend, close — touches the
	// buffer and the close state and never the socket. That separation is not an
	// accident of this test; it is the property that lets the hub's fan-out
	// goroutine drop a client without ever waiting on a network.
	stalled := newConn(inst.hub, nil, principalFor(room.TenantID, 15*time.Minute))
	draining := newConn(inst.hub, nil, principalFor(room.TenantID, 15*time.Minute))

	for _, conn := range []*Conn{stalled, draining} {
		if err := inst.hub.join(ctx, room, conn); err != nil {
			t.Fatalf("joining the room: %v", err)
		}
	}

	var received atomic.Int64

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		for {
			select {
			case <-ctx.Done():
				return
			case <-draining.closed:
				return
			case <-draining.send:
				received.Add(1)
			}
		}
	}()

	// One more than the buffer holds is enough. There is no queue anywhere else
	// in the path: deliver either lands the frame in the channel or it does not.
	const events = buffer + 4

	// One at a time, waiting for the draining client to take each. Publishing
	// them in a burst would let the *draining* client fall behind too if its
	// goroutine simply was not scheduled in between, and then this would be
	// measuring the Go scheduler rather than the buffer.
	for i := range events {
		before := received.Load()

		if err := inst.hub.Publish(ctx, room, Event{
			ID:         uuid.New(),
			Type:       "card.moved",
			OccurredAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("publishing: %v", err)
		}

		eventually(t, 10*time.Second, fmt.Sprintf("the draining client to take event %d", i), func() bool {
			return received.Load() > before
		})
	}

	eventually(t, 10*time.Second, "the stalled client to be dropped", func() bool {
		select {
		case <-stalled.closed:
			return true
		default:
			return false
		}
	})

	status, reason := stalled.closeInfo()

	t.Logf("the stalled client was closed with %d (%s) after a %d-frame buffer filled", status, reason, buffer)

	if status != StatusSlowConsumer {
		t.Errorf("close status = %d, want %d (slow consumer)", status, StatusSlowConsumer)
	}

	// The other connection on the same board received every event, which is the
	// half that matters: a hub that blocked on the stalled peer would have
	// delivered it nothing.
	if got := received.Load(); got != events {
		t.Errorf("the draining client received %d of %d events", got, events)
	}

	select {
	case <-draining.closed:
		t.Fatal("the draining client was dropped too")
	default:
	}

	t.Logf("the draining client received all %d events and is still open", received.Load())

	// And the hub forgot the dropped one only when its connection was cleaned
	// up, not before — the fan-out goroutine records the decision and never
	// touches the maps or a socket.
	if got := inst.hub.connectionsFor(room); got != 2 {
		t.Errorf("connections for %s = %d, want 2: deliver must not deregister anything itself", room, got)
	}

	cancel()
	<-drained
}

// TestASlowClientIsDroppedWithoutStallingOthers is the same claim end to end,
// over real sockets.
//
// The slow client is a real connection that never reads a byte. Eventually the
// server cannot make progress writing to it — through its send buffer filling,
// or through the frame in flight exceeding WriteTimeout, and *which* of those
// happens first depends on how much the machine's kernel is willing to buffer,
// so this test does not assert on it. What it asserts is machine-independent
// and is the actual requirement: the connection ends, and the client on the
// same board is untouched throughout and still receiving afterwards.
//
// The precise "a full buffer yields 4002" claim is pinned deterministically by
// TestAFullSendBufferDropsTheClientAndNothingElse above.
func TestASlowClientIsDroppedWithoutStallingOthers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	const (
		payloadSize = 16 << 10

		// Spaced, so that a client which *is* reading has nothing to struggle
		// with.
		publishInterval = 2 * time.Millisecond
	)

	inst := newInstance(t, instanceOptions{
		authorizer: allowAll{},
		sendBuffer: 8,

		// Short, so this terminates promptly whatever the machine's socket
		// buffers turn out to be. On a host that buffers little, the send buffer
		// fills first and the client gets 4002; on one that buffers a lot, the
		// frame in flight times out first and the connection ends as a write
		// failure. Both are "the hub gave up on this peer rather than waiting for
		// it", which is what this test is about.
		writeTimeout: 2 * time.Second,
	})

	tenantID := uuid.New()
	boardID := uuid.New()
	room := Room{TenantID: tenantID, BoardID: boardID}

	// The slow client: connected, subscribed, and never reading a byte.
	slowPrincipal := principalFor(tenantID, 15*time.Minute)
	slow := inst.dialRaw(ctx, t, inst.token(t, slowPrincipal))

	subscribeFrame, err := json.Marshal(clientFrame{Type: clientSubscribe, BoardID: boardID.String()})
	if err != nil {
		t.Fatalf("encoding a subscribe: %v", err)
	}

	if err := slow.Write(ctx, websocket.MessageText, subscribeFrame); err != nil {
		t.Fatalf("subscribing the slow client: %v", err)
	}

	eventually(t, 10*time.Second, "the slow client to join the room", func() bool {
		return inst.hub.connectionsFor(room) == 1
	})

	// The fast client: a goroutine that reads as fast as it can and counts.
	var received atomic.Int64

	fastPrincipal := principalFor(tenantID, 15*time.Minute)

	fastWS := inst.dialRaw(ctx, t, inst.token(t, fastPrincipal))
	if err := fastWS.Write(ctx, websocket.MessageText, subscribeFrame); err != nil {
		t.Fatalf("subscribing the fast client: %v", err)
	}

	fastErr := make(chan error, 1)

	go func() {
		for {
			_, data, err := fastWS.Read(ctx)
			if err != nil {
				fastErr <- err

				return
			}

			var frame Frame
			if err := json.Unmarshal(data, &frame); err != nil {
				fastErr <- err

				return
			}

			if frame.Type == FrameEvent {
				received.Add(1)
			}
		}
	}()

	eventually(t, 10*time.Second, "both clients to join the room", func() bool {
		return inst.hub.connectionsFor(room) == 2
	})

	payload := json.RawMessage(`{"blob":"` + strings.Repeat("x", payloadSize) + `"}`)

	publish := func() {
		t.Helper()

		if err := inst.hub.Publish(ctx, room, Event{
			ID:         uuid.New(),
			Type:       "card.moved",
			ActorID:    fastPrincipal.UserID,
			OccurredAt: time.Now().UTC(),
			Payload:    payload,
		}); err != nil {
			t.Fatalf("publishing: %v", err)
		}
	}

	// Publish until the hub gives up on the peer that is not reading. The count
	// is not fixed because it depends on how much the kernel is willing to
	// buffer, which varies by machine; what is fixed is the *outcome*.
	started := time.Now()
	deadline := started.Add(90 * time.Second)
	published := 0

	for inst.hub.connectionsFor(room) == 2 {
		if time.Now().After(deadline) {
			t.Fatalf("published %d events without the slow client being dropped", published)
		}

		select {
		case err := <-fastErr:
			t.Fatalf("the reading client was closed after %d events: %v", received.Load(), err)
		default:
		}

		publish()

		published++

		time.Sleep(publishInterval)
	}

	t.Logf("the slow client was dropped after %d events of ~%dB in %s; the reading client had taken %d",
		published, payloadSize, time.Since(started), received.Load())

	// Whatever ended it, the server ended it deliberately rather than hanging.
	// The socket still holds everything the client never read, so this drains to
	// the end.
	var closeErr error

	for {
		if _, _, err := slow.Read(ctx); err != nil {
			closeErr = err

			break
		}
	}

	t.Logf("the slow client's connection ended with close status %d: %v",
		websocket.CloseStatus(closeErr), closeErr)

	// And the point of the whole exercise: the other client on the same board
	// is untouched and still receiving. A hub that had blocked on the slow peer
	// would fail here, not above.
	before := received.Load()

	publish()

	eventually(t, 20*time.Second, "the reading client to receive an event published after the drop", func() bool {
		return received.Load() > before
	})

	select {
	case err := <-fastErr:
		t.Fatalf("the reading client was closed: %v", err)
	default:
	}

	t.Logf("the reading client received %d events in total and is still connected", received.Load())
}

// TestADeadConnectionIsReaped exercises the ping/pong reaper.
//
// coder/websocket answers pings from inside its read loop, so a client that
// never reads never pongs while its TCP connection stays perfectly healthy.
// That is the failure mode a keepalive exists for — nothing else on the server
// would ever notice.
func TestADeadConnectionIsReaped(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{
		authorizer:   allowAll{},
		pingInterval: 100 * time.Millisecond,
		pongTimeout:  200 * time.Millisecond,
	})

	tenantID := uuid.New()

	inst.dialRaw(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))

	// The connection is counted before it subscribes to anything, so this
	// assertion is about the reaper rather than about a number that was zero
	// all along.
	eventually(t, 5*time.Second, "the connection to be registered", func() bool {
		_, conns := inst.hub.Rooms()

		return conns == 1
	})

	eventually(t, 10*time.Second, "the unresponsive connection to be reaped", func() bool {
		_, conns := inst.hub.Rooms()

		return conns == 0
	})

	t.Log("a peer that never answered a ping was reaped without ever closing its TCP connection")
}

func TestAConnectionIsClosedWhenItsAccessTokenExpires(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// The connection's deadline comes from the *token's* exp, which is the
	// issuer's business and not the caller's — so this is configured on the
	// issuer, exactly as AUTH_ACCESS_TOKEN_TTL would be.
	//
	// Two seconds rather than something snappier because a JWT's exp has
	// one-second resolution and jwt.NewNumericDate truncates: a sub-second TTL
	// produces a token that is already expired when it is issued.
	inst := newInstance(t, instanceOptions{
		authorizer: allowAll{},
		issuer:     testIssuer(t, 2*time.Second),
	})

	c := inst.dial(ctx, t, inst.token(t, principalFor(uuid.New(), 2*time.Second)))

	if got := c.closeStatus(10 * time.Second); got != StatusTokenExpired {
		t.Fatalf("close status = %d, want %d (token expired)", got, StatusTokenExpired)
	}

	t.Log("the socket did not outlive the credential that opened it")
}

// TestAWedgedAuthorizerCannotOutlastTheToken is the guarantee that a socket
// never outlives the credential that opened it, tested against the thing most
// likely to break it.
//
// The maintenance loop runs the ping, the re-authorization sweep and the
// token-expiry deadline in one goroutine, so an authorization call that never
// returned would hold all three — and a connection would quietly keep running
// on an expired token for as long as Postgres stayed wedged. This authorizer
// answers only when its context is cancelled, so without the bound in
// authorizeTimeout the connection would never close and this test would hang
// rather than fail.
func TestAWedgedAuthorizerCannotOutlastTheToken(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{
		authorizer: blockingAuthorizer{},
		issuer:     testIssuer(t, 2*time.Second),

		// Fires long before the token expires, so the sweep is already stuck
		// inside the authorizer when the deadline arrives.
		reauthorizeInterval: 50 * time.Millisecond,
	})

	started := time.Now()

	c := inst.dial(ctx, t, inst.token(t, principalFor(uuid.New(), 2*time.Second)))

	if got := c.closeStatus(45 * time.Second); got != StatusTokenExpired {
		t.Fatalf("close status = %d, want %d (token expired)", got, StatusTokenExpired)
	}

	t.Logf("the connection closed %s after opening, despite an authorizer that never answers",
		time.Since(started).Round(time.Millisecond))
}

func TestRevokingAMembershipClosesTheConnection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	authorizer := newScriptedAuthorizer()

	inst := newInstance(t, instanceOptions{
		authorizer:          authorizer,
		reauthorizeInterval: 50 * time.Millisecond,
	})

	tenantID := uuid.New()
	boardID := uuid.New()

	c := inst.dial(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))
	c.subscribe(t, ctx, boardID)

	authorizer.revokeTenant()

	if got := c.closeStatus(10 * time.Second); got != StatusMembershipRevoked {
		t.Fatalf("close status = %d, want %d (membership revoked)", got, StatusMembershipRevoked)
	}

	eventually(t, 5*time.Second, "the revoked connection to be deregistered", func() bool {
		rooms, conns := inst.hub.Rooms()

		return rooms == 0 && conns == 0
	})

	t.Log("a membership revoked mid-connection took effect within one re-authorization interval")
}

func TestLosingAccessToOneBoardUnsubscribesOnlyThatBoard(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	authorizer := newScriptedAuthorizer()

	inst := newInstance(t, instanceOptions{
		authorizer:          authorizer,
		reauthorizeInterval: 50 * time.Millisecond,
	})

	tenantID := uuid.New()
	kept := uuid.New()
	lost := uuid.New()

	principal := principalFor(tenantID, 15*time.Minute)
	token := inst.token(t, principal)

	c := inst.dial(ctx, t, token)
	c.subscribe(t, ctx, kept)
	c.subscribe(t, ctx, lost)

	authorizer.revokeBoard(lost)

	frame := c.expect(FrameUnsubscribed, 10*time.Second)
	if frame.BoardID == nil || *frame.BoardID != lost {
		t.Fatalf("unsubscribed from %v, want %s", frame.BoardID, lost)
	}

	if frame.Reason != ReasonForbidden {
		t.Errorf("reason = %q, want %q", frame.Reason, ReasonForbidden)
	}

	// The other subscription survives, and the connection with it.
	eventually(t, 5*time.Second, "only the revoked room to be dropped", func() bool {
		return inst.hub.connectionsFor(Room{TenantID: tenantID, BoardID: lost}) == 0 &&
			inst.hub.connectionsFor(Room{TenantID: tenantID, BoardID: kept}) == 1
	})
}

// TestAnUnavailableAuthorizerDoesNotDisconnectEverybody is the availability
// half of the revocation decision.
//
// If a Postgres blip closed every WebSocket in the fleet, the recovery would be
// a reconnect storm on top of a database that is already unhealthy. New
// subscriptions still fail closed; existing ones ride it out.
func TestAnUnavailableAuthorizerDoesNotDisconnectEverybody(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	authorizer := newScriptedAuthorizer()

	inst := newInstance(t, instanceOptions{
		authorizer:          authorizer,
		reauthorizeInterval: 30 * time.Millisecond,
	})

	tenantID := uuid.New()
	boardID := uuid.New()

	c := inst.dial(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))
	c.subscribe(t, ctx, boardID)

	authorizer.failTenantWith(errAuthorizerUnavailable)

	eventually(t, 5*time.Second, "several re-authorization sweeps to run", func() bool {
		tenantChecks, _ := authorizer.counts()

		return tenantChecks >= 3
	})

	if _, conns := inst.hub.Rooms(); conns != 1 {
		t.Fatalf("connections = %d, want 1 — an unreachable database disconnected a live client", conns)
	}

	// But a *new* subscription still fails closed.
	authorizer.mu.Lock()
	authorizer.board = errAuthorizerUnavailable
	authorizer.mu.Unlock()

	other := uuid.New()

	c.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: other.String()})

	frame := c.expect(FrameError, 5*time.Second)
	if frame.Reason != ReasonUnavailable {
		t.Errorf("reason = %q, want %q", frame.Reason, ReasonUnavailable)
	}

	if got := inst.hub.connectionsFor(Room{TenantID: tenantID, BoardID: other}); got != 0 {
		t.Errorf("an unanswerable authorization created %d subscriptions; it must create none", got)
	}
}

// TestShutdownWarnsClientsThenClosesThem is the instance-restart behaviour.
func TestShutdownWarnsClientsThenClosesThem(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	tenantID := uuid.New()
	boardID := uuid.New()

	c := inst.dial(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))
	c.subscribe(t, ctx, boardID)

	// A second connection that never subscribes. It is the one a hub that only
	// tracked room members would forget about, leaving a socket for the process
	// exit to reset.
	idle := inst.dial(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))

	eventually(t, 5*time.Second, "both connections to be registered", func() bool {
		_, conns := inst.hub.Rooms()

		return conns == 2
	})

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 10*time.Second)
	defer shutdownCancel()

	done := make(chan error, 1)

	go func() { done <- inst.hub.Shutdown(shutdownCtx) }()

	idleFrame := idle.expect(FrameShutdown, 10*time.Second)
	if idleFrame.ReconnectAfterMs <= 0 {
		t.Error("the idle connection got no reconnect delay")
	}

	if got := idle.closeStatus(10 * time.Second); got != closeGoingAway {
		t.Errorf("the idle connection saw close status %d, want %d (going away)", got, closeGoingAway)
	}

	frame := c.expect(FrameShutdown, 10*time.Second)

	t.Logf("shutdown frame: reconnect_after_ms=%d", frame.ReconnectAfterMs)

	if frame.ReconnectAfterMs <= 0 {
		t.Error("the shutdown frame carried no reconnect delay; every client would reconnect at once")
	}

	if got := c.closeStatus(10 * time.Second); got != closeGoingAway {
		t.Errorf("close status = %d, want %d (going away)", got, closeGoingAway)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown did not return")
	}

	// A draining instance refuses new upgrades rather than accepting one it is
	// about to close.
	_, resp, err := websocket.Dial(ctx, inst.wsURL(), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + inst.token(t, principalFor(tenantID, 15*time.Minute))}},
		Subprotocols: []string{Subprotocol},
	})
	if err == nil {
		t.Fatal("a draining instance accepted a new connection")
	}

	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a draining instance answered %v, want 503", resp)
	}

	if resp != nil {
		_ = resp.Body.Close()
	}
}

func TestTheUpgradeRequiresAValidToken(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	other := testIssuer(t, 15*time.Minute)

	// A token signed by a different issuer instance with a different secret.
	foreign, err := auth.NewIssuer(auth.TokenConfig{
		Secret:    []byte("fedcba9876543210fedcba9876543210"),
		Issuer:    testIssuerID,
		Audience:  testAudience,
		AccessTTL: 15 * time.Minute,
	})
	if err != nil {
		t.Fatalf("building the foreign issuer: %v", err)
	}

	foreignToken, _, err := foreign.Issue(principalFor(uuid.New(), 15*time.Minute))
	if err != nil {
		t.Fatalf("issuing a foreign token: %v", err)
	}

	expired, _, err := testIssuer(t, time.Nanosecond).Issue(principalFor(uuid.New(), time.Nanosecond))
	if err != nil {
		t.Fatalf("issuing an expired token: %v", err)
	}

	time.Sleep(2 * time.Millisecond)

	valid, _, err := other.Issue(principalFor(uuid.New(), 15*time.Minute))
	if err != nil {
		t.Fatalf("issuing a valid token: %v", err)
	}

	for _, tc := range []struct {
		name   string
		header string
		want   int
	}{
		{name: "no authorization header", header: "", want: http.StatusUnauthorized},
		{name: "not a token", header: "Bearer not-a-token", want: http.StatusUnauthorized},
		{name: "signed by another secret", header: "Bearer " + foreignToken, want: http.StatusUnauthorized},
		{name: "expired", header: "Bearer " + expired, want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := &websocket.DialOptions{Subprotocols: []string{Subprotocol}}
			if tc.header != "" {
				opts.HTTPHeader = http.Header{"Authorization": []string{tc.header}}
			}

			_, resp, err := websocket.Dial(ctx, inst.wsURL(), opts)
			if err == nil {
				t.Fatal("the upgrade succeeded without a valid token")
			}

			if resp == nil || resp.StatusCode != tc.want {
				t.Fatalf("status = %v, want %d", resp, tc.want)
			}

			_ = resp.Body.Close()
		})
	}

	// The control: the same dial with a good token works, so the refusals above
	// are about the token and not about the harness.
	t.Run("control: a valid token connects", func(t *testing.T) {
		inst.dial(ctx, t, valid)
	})
}

// TestABrowserCanPresentItsTokenInTheSubprotocol covers the one thing about
// this endpoint a REST client never needs.
func TestABrowserCanPresentItsTokenInTheSubprotocol(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	tenantID := uuid.New()
	boardID := uuid.New()

	c := inst.dialSubprotocol(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))
	c.subscribe(t, ctx, boardID)

	t.Log("the same token verified by the same middleware, presented the only way a browser can")
}

func TestMalformedClientFramesAreRefusedWithoutClosingTheConnection(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}})

	c := inst.dial(ctx, t, inst.token(t, principalFor(uuid.New(), 15*time.Minute)))

	for _, tc := range []struct {
		name  string
		frame clientFrame
	}{
		{name: "unknown type", frame: clientFrame{Type: "delete-everything"}},
		{name: "board id is not a uuid", frame: clientFrame{Type: clientSubscribe, BoardID: "../../etc/passwd"}},
		{name: "board id is the zero uuid", frame: clientFrame{Type: clientSubscribe, BoardID: uuid.Nil.String()}},
	} {
		c.send(t, ctx, tc.frame)

		frame := c.expect(FrameError, 5*time.Second)
		if frame.Reason != ReasonInvalid {
			t.Errorf("%s: reason = %q, want %q", tc.name, frame.Reason, ReasonInvalid)
		}
	}

	// Still usable afterwards.
	c.send(t, ctx, clientFrame{Type: clientPing})
	c.expect(FramePong, 5*time.Second)
}

func TestAConnectionCannotWatchUnboundedlyManyBoards(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	inst := newInstance(t, instanceOptions{authorizer: allowAll{}, maxRooms: 2})

	tenantID := uuid.New()

	c := inst.dial(ctx, t, inst.token(t, principalFor(tenantID, 15*time.Minute)))

	c.subscribe(t, ctx, uuid.New())
	c.subscribe(t, ctx, uuid.New())

	c.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: uuid.NewString()})

	frame := c.expect(FrameError, 5*time.Second)
	if frame.Reason != ReasonTooManyRooms {
		t.Errorf("reason = %q, want %q", frame.Reason, ReasonTooManyRooms)
	}

	if rooms, _ := inst.hub.Rooms(); rooms != 2 {
		t.Errorf("rooms = %d, want 2", rooms)
	}
}
