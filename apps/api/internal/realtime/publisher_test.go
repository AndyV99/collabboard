package realtime

// The adapter between the HTTP write path and the fan-out.
//
// hub_test.go and bola_test.go exercise it end to end through a real card move
// over a real socket. This file covers the two things that are easier to state
// than to reach from there: that the room is built from the event and from
// nothing else, and what happens to a payload that is too large to broadcast.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
)

// newBusHub builds a hub over a memory bus and returns a second broker attached
// to it, standing in for another instance.
func newBusHub(t *testing.T) (*Hub, *MemoryBroker) {
	t.Helper()

	bus := NewMemoryBus()

	hub, err := NewHub(HubConfig{Broker: bus.Broker(), Authorizer: allowAll{}, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("building the hub: %v", err)
	}

	other := bus.Broker()

	t.Cleanup(func() {
		if err := hub.Shutdown(context.Background()); err != nil {
			t.Errorf("shutting the hub down: %v", err)
		}

		if err := other.Close(); err != nil {
			t.Errorf("closing the second broker: %v", err)
		}
	})

	return hub, other
}

// TestTheWritePathsEventIsAddressedByTheRoomAndCarriesItsActor is the contract
// between internal/api and this package, asserted on the bytes that leave the
// instance.
func TestTheWritePathsEventIsAddressedByTheRoomAndCarriesItsActor(t *testing.T) {
	t.Parallel()

	hub, other := newBusHub(t)

	tenantID, boardID, actorID := uuid.New(), uuid.New(), uuid.New()
	room := Room{TenantID: tenantID, BoardID: boardID}

	if err := other.Subscribe(t.Context(), room); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	err := hub.EventPublisher().PublishBoardEvent(t.Context(), api.BoardEvent{
		TenantID: tenantID,
		ActorID:  actorID,
		BoardID:  boardID,
		Type:     "card.moved",
		Payload:  map[string]any{"card": map[string]any{"id": "abc"}},
	})
	if err != nil {
		t.Fatalf("publishing: %v", err)
	}

	msg := <-other.Messages()

	if msg.Room != room {
		t.Errorf("room = %s, want %s", msg.Room, room)
	}

	var frame Frame
	if err := json.Unmarshal(msg.Payload, &frame); err != nil {
		t.Fatalf("decoding the frame: %v", err)
	}

	t.Logf("the frame on the wire: %s", msg.Payload)

	switch {
	case frame.Type != FrameEvent:
		t.Errorf("frame type = %q, want %q", frame.Type, FrameEvent)
	case frame.BoardID == nil || *frame.BoardID != boardID:
		t.Errorf("frame board = %v, want %s", frame.BoardID, boardID)
	case frame.Event == nil:
		t.Fatal("the frame carries no event")
	}

	if frame.Event.ActorID != actorID {
		t.Errorf("actor = %s, want %s", frame.Event.ActorID, actorID)
	}

	if frame.Event.ID == uuid.Nil {
		t.Error("the event has no id; a client cannot deduplicate across a reconnect without one")
	}

	if frame.Event.OccurredAt.IsZero() {
		t.Error("the event has no timestamp")
	}

	// No tenant anywhere in what a client sees. A routing bug must not be able
	// to put another organization's id in somebody's browser.
	if strings.Contains(string(msg.Payload), tenantID.String()) {
		t.Errorf("the client frame names a tenant: %s", msg.Payload)
	}
}

// TestAnOversizedPayloadIsRefusedRatherThanBroadcast covers the backstop.
//
// Payloads here are built by this service from rows it just wrote, so exceeding
// the cap means a bug upstream — but the consequence of broadcasting it anyway
// is every connection's send buffer holding sixty-four copies of it, which is
// the wrong thing to discover in production.
func TestAnOversizedPayloadIsRefusedRatherThanBroadcast(t *testing.T) {
	t.Parallel()

	hub, other := newBusHub(t)

	room := Room{TenantID: uuid.New(), BoardID: uuid.New()}

	if err := other.Subscribe(t.Context(), room); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	err := hub.EventPublisher().PublishBoardEvent(t.Context(), api.BoardEvent{
		TenantID: room.TenantID,
		ActorID:  uuid.New(),
		BoardID:  room.BoardID,
		Type:     "card.updated",
		Payload:  map[string]any{"card": map[string]any{"description": strings.Repeat("x", maxEventPayloadBytes+1)}},
	})
	if err == nil {
		t.Fatal("an oversized payload was broadcast")
	}

	t.Logf("an oversized payload was refused: %v", err)

	select {
	case msg := <-other.Messages():
		t.Fatalf("it went out anyway: %d bytes", len(msg.Payload))
	default:
	}
}

// TestPublishingAnEventWithNoBoardIsRefused is the wiring check.
//
// uuid.Nil is a syntactically valid id that addresses a room nobody can ever be
// authorized for, so an event carrying one would be silently delivered to
// nothing. Refusing it turns a wiring mistake into an error in a log.
func TestPublishingAnEventWithNoBoardIsRefused(t *testing.T) {
	t.Parallel()

	hub, _ := newBusHub(t)

	for _, tc := range []struct {
		name  string
		event api.BoardEvent
	}{
		{name: "no board", event: api.BoardEvent{TenantID: uuid.New(), Type: "card.moved"}},
		{name: "no tenant", event: api.BoardEvent{BoardID: uuid.New(), Type: "card.moved"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := hub.EventPublisher().PublishBoardEvent(t.Context(), tc.event); err == nil {
				t.Fatal("an incomplete room was published to")
			}
		})
	}
}
