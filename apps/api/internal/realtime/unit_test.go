package realtime

// The small, fast assertions: addressing, wiring checks, and the transport
// envelope's misrouting guard.

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func TestRoomChannelSeparatesTenants(t *testing.T) {
	t.Parallel()

	boardID := uuid.New()
	first := Room{TenantID: uuid.New(), BoardID: boardID}
	second := Room{TenantID: uuid.New(), BoardID: boardID}

	if first.Channel() == second.Channel() {
		t.Fatalf("the same board id in two tenants produced one channel: %s", first.Channel())
	}

	for _, room := range []Room{first, second} {
		if !strings.HasPrefix(room.Channel(), channelPrefix+".") {
			t.Errorf("channel %q is outside the %q namespace", room.Channel(), channelPrefix)
		}

		if !strings.Contains(room.Channel(), room.TenantID.String()) {
			t.Errorf("channel %q does not name its tenant", room.Channel())
		}
	}
}

func TestAnIncompleteRoomIsRejected(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		room Room
	}{
		{name: "no tenant", room: Room{BoardID: uuid.New()}},
		{name: "no board", room: Room{TenantID: uuid.New()}},
		{name: "neither", room: Room{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.room.valid() {
				t.Fatalf("%s reported itself valid", tc.room)
			}
		})
	}
}

func TestNewHubRequiresItsDependencies(t *testing.T) {
	t.Parallel()

	bus := NewMemoryBus()

	for _, tc := range []struct {
		name string
		cfg  HubConfig
	}{
		{name: "no broker", cfg: HubConfig{Authorizer: allowAll{}, Logger: discardLogger()}},
		{name: "no authorizer", cfg: HubConfig{Broker: bus.Broker(), Logger: discardLogger()}},
		{name: "no logger", cfg: HubConfig{Broker: bus.Broker(), Authorizer: allowAll{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewHub(tc.cfg); err == nil {
				t.Fatal("a hub was built without one of its dependencies")
			}
		})
	}
}

func TestPublishingToAnIncompleteRoomIsRefused(t *testing.T) {
	t.Parallel()

	bus := NewMemoryBus()

	hub, err := NewHub(HubConfig{Broker: bus.Broker(), Authorizer: allowAll{}, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("building the hub: %v", err)
	}

	t.Cleanup(func() {
		if err := hub.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting down: %v", err)
		}
	})

	if err := hub.Publish(t.Context(), Room{BoardID: uuid.New()}, Event{}); err == nil {
		t.Fatal("an event was published to a room with no tenant")
	}
}

// TestAMisroutedTransportMessageIsDropped covers the check that exists because
// a delivered mismatch would be a cross-tenant leak.
func TestAMisroutedTransportMessageIsDropped(t *testing.T) {
	t.Parallel()

	broker := &RedisBroker{
		logger:   discardLogger(),
		messages: make(chan Message, 4),
		waiters:  map[string][]chan struct{}{},
		done:     make(chan struct{}),
	}

	honest := Room{TenantID: uuid.New(), BoardID: uuid.New()}
	foreign := Room{TenantID: uuid.New(), BoardID: uuid.New()}

	encode := func(room Room) string {
		encoded, err := json.Marshal(transportEnvelope{
			TenantID: room.TenantID,
			BoardID:  room.BoardID,
			Payload:  json.RawMessage(`{"type":"event"}`),
		})
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}

		return string(encoded)
	}

	// An envelope addressed to another room, delivered on this room's channel.
	// Exactly what a routing bug — or something else publishing into this
	// namespace — would look like.
	broker.handleMessage(&redis.Message{Channel: honest.Channel(), Payload: encode(foreign)})

	// And one that is not decodable at all.
	broker.handleMessage(&redis.Message{Channel: honest.Channel(), Payload: "{"})

	select {
	case msg := <-broker.messages:
		t.Fatalf("a misrouted message was forwarded: %+v", msg)
	default:
	}

	// The control: an honest message does get through, so the two refusals
	// above are about the mismatch and not about the harness.
	broker.handleMessage(&redis.Message{Channel: honest.Channel(), Payload: encode(honest)})

	select {
	case msg := <-broker.messages:
		if msg.Room != honest {
			t.Fatalf("room = %s, want %s", msg.Room, honest)
		}
	default:
		t.Fatal("an honest message was dropped")
	}
}

func TestTheMemoryBusDeliversToEveryAttachedBroker(t *testing.T) {
	t.Parallel()

	bus := NewMemoryBus()

	publisher := bus.Broker()
	subscribed := bus.Broker()
	uninterested := bus.Broker()

	t.Cleanup(func() {
		for _, broker := range []*MemoryBroker{publisher, subscribed, uninterested} {
			if err := broker.Close(); err != nil {
				t.Errorf("closing a broker: %v", err)
			}
		}
	})

	room := Room{TenantID: uuid.New(), BoardID: uuid.New()}

	if err := subscribed.Subscribe(t.Context(), room); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	if err := publisher.Publish(t.Context(), room, []byte(`{"type":"event"}`)); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	select {
	case msg := <-subscribed.Messages():
		if msg.Room != room {
			t.Fatalf("room = %s, want %s", msg.Room, room)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the subscribed broker received nothing")
	}

	select {
	case msg := <-uninterested.Messages():
		t.Fatalf("a broker with no subscription received %+v", msg)
	default:
	}

	// The publisher is not subscribed either, which is what makes "publish does
	// not deliver locally, the subscription does" observable.
	select {
	case msg := <-publisher.Messages():
		t.Fatalf("the publisher received its own message without subscribing: %+v", msg)
	default:
	}
}

func TestABrokerRefusesWorkAfterClose(t *testing.T) {
	t.Parallel()

	bus := NewMemoryBus()
	broker := bus.Broker()

	if err := broker.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	room := Room{TenantID: uuid.New(), BoardID: uuid.New()}

	for name, call := range map[string]func() error{
		"subscribe":   func() error { return broker.Subscribe(t.Context(), room) },
		"unsubscribe": func() error { return broker.Unsubscribe(t.Context(), room) },
		"publish":     func() error { return broker.Publish(t.Context(), room, nil) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s succeeded on a closed broker", name)
		}
	}

	// Close is idempotent, because Shutdown and a deferred cleanup will both
	// call it.
	if err := broker.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestTheClientFrameCarriesNoTenant(t *testing.T) {
	t.Parallel()

	// A structural assertion, not a behavioural one: it is what stops somebody
	// adding a tenant field to the client protocol in six months without
	// noticing that the whole isolation argument rests on there not being one.
	encoded, err := json.Marshal(Frame{
		Type:    FrameEvent,
		BoardID: &uuid.Max,
		Event:   &Event{ID: uuid.New(), Type: "card.moved"},
	})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	for _, forbidden := range []string{"tenant", "org"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Errorf("a server frame mentions %q: %s", forbidden, encoded)
		}
	}

	var frame clientFrame
	if err := json.Unmarshal([]byte(`{"type":"subscribe","board_id":"x","tenant_id":"y","org":"z"}`), &frame); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	// The extra fields decode into nothing, because there is nothing for them
	// to decode into.
	if frame.Type != clientSubscribe || frame.BoardID != "x" {
		t.Fatalf("decoded %+v", frame)
	}
}

func TestEncodePayloadOmitsAnEmptyPayload(t *testing.T) {
	t.Parallel()

	encoded, err := encodePayload(nil)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	if encoded != nil {
		t.Fatalf("an empty payload encoded to %q", encoded)
	}

	encoded, err = encodePayload(map[string]any{"card_id": "c1"})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	if !json.Valid(encoded) {
		t.Fatalf("payload is not valid json: %q", encoded)
	}
}

func TestTheReconnectHintIsJittered(t *testing.T) {
	t.Parallel()

	hub := &Hub{cfg: HubConfig{ShutdownReconnectHint: time.Second, Logger: discardLogger()}}

	seen := map[time.Duration]struct{}{}

	for range 50 {
		hint := hub.reconnectHint()

		if hint < time.Second || hint >= 2*time.Second {
			t.Fatalf("hint %s is outside [1s, 2s)", hint)
		}

		seen[hint] = struct{}{}
	}

	// A constant would produce one value, and a rolling deploy would produce a
	// synchronised reconnect.
	if len(seen) < 10 {
		t.Fatalf("only %d distinct delays in 50 draws; the hint is not jittered", len(seen))
	}
}

func TestRemovingASubscriptionWaiterIsSafe(t *testing.T) {
	t.Parallel()

	broker := &RedisBroker{
		logger:  slog.New(slog.DiscardHandler),
		waiters: map[string][]chan struct{}{},
		done:    make(chan struct{}),
	}

	const channel = "collabboard.rt.v1.a.b"

	first := broker.addWaiter(channel)
	second := broker.addWaiter(channel)

	broker.removeWaiter(channel, first)

	if got := len(broker.waiters[channel]); got != 1 {
		t.Fatalf("waiters = %d, want 1", got)
	}

	broker.removeWaiter(channel, second)

	if _, ok := broker.waiters[channel]; ok {
		t.Fatal("an empty waiter list was left behind")
	}

	// Removing one that is already gone must not panic or resurrect the key.
	broker.removeWaiter(channel, first)
}
