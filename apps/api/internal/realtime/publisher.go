package realtime

// The adapter that lets the HTTP write path broadcast without importing this
// package.
//
// internal/realtime imports internal/api (for PrincipalFromContext), so the
// dependency cannot run the other way: internal/api declares
// [api.EventPublisher] in terms of its own types and this file is the one
// implementation. The card handler therefore names no realtime type, and this
// package stays the only thing that knows what a Room or an Event is.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
)

// maxEventPayloadBytes caps one event's rendered payload.
//
// It is a backstop rather than a defence: every payload is built by this service
// from rows it just wrote, and its size is already bounded by the API's own
// field limits (a title of 200 runes, a description of 10 000). Four bytes per
// rune puts the largest legitimate card event around 40 KiB, so this leaves
// headroom and still bounds what one connection's send buffer can hold —
// SendBuffer (64) frames of it, which is the number that matters when a client
// stops reading.
//
// Exceeding it means a bug upstream, so the event is dropped with a loud error
// rather than allowed to become the fleet's memory profile. The client's board
// is stale until it re-fetches, which is the same recovery path every other
// publish failure uses.
const maxEventPayloadBytes = 64 << 10

// EventPublisher returns the hub as the write path's broadcaster.
//
// A method rather than a package function so that cmd/api's wiring reads the
// same as the connect handler's: one hub, two things taken off it.
func (h *Hub) EventPublisher() api.EventPublisher {
	return eventPublisher{hub: h}
}

type eventPublisher struct{ hub *Hub }

// PublishBoardEvent renders a committed change and fans it out to the board's
// room on every instance.
//
// It is called *after* the transaction committed and *before* the HTTP response
// is written — see internal/api/events.go for why both halves of that matter.
// The returned error is logged by the caller and never fails the request: the
// write already happened, and refusing it now would be a lie.
func (p eventPublisher) PublishBoardEvent(ctx context.Context, event api.BoardEvent) error {
	payload, err := encodePayload(event.Payload)
	if err != nil {
		return err
	}

	if len(payload) > maxEventPayloadBytes {
		return fmt.Errorf("realtime: %s payload is %d bytes, over the %d limit",
			event.Type, len(payload), maxEventPayloadBytes)
	}

	// principalRoom's job, done from the caller's principal one frame up: the
	// tenant half of the key is never reachable from a request, here or on the
	// subscribe path.
	room := Room{TenantID: event.TenantID, BoardID: event.BoardID}

	return p.hub.Publish(ctx, room, Event{
		ID:   uuid.New(),
		Type: event.Type,
		// From the authenticated principal, never from a request body: an event
		// that could name its own actor would let any member forge another
		// member's action in every open browser on the board.
		ActorID:    event.ActorID,
		OccurredAt: p.hub.cfg.now().UTC(),
		Payload:    payload,
	})
}

// encodePayload renders the payload once, on the publishing instance, so the
// fan-out path carries bytes rather than a value every instance would have to
// encode again per recipient.
func encodePayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encoding the event payload: %w", err)
	}

	return encoded, nil
}
