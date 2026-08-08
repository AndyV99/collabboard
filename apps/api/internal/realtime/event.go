package realtime

// The wire formats. Three of them, and keeping them separate is load-bearing.
//
//  1. [Room] — the addressing unit. A (tenant, board) pair, and the Redis
//     channel name is derived from it. Tenant is in the key even though board
//     ids are globally unique uuids, so that a subscriber registered under
//     tenant A cannot be reached by an event addressed to tenant B even if a
//     board id were guessed, reused or collided. It costs nothing and it means
//     the isolation claim holds at the map key, not only at the check that
//     precedes it.
//
//  2. [transportEnvelope] — what travels through Redis. It carries the room
//     *again*, next to the payload, so a receiver can check that the message it
//     got on a channel is addressed to that channel. Redis routes by channel
//     name and there is no reason to expect it to lie, but the whole point of
//     this package's tenant handling is that misrouting must be detectable
//     rather than assumed away, and the check is one comparison.
//
//  3. [Frame] — what a client sees. It has no tenant field. A client already
//     knows its own tenant, so including it would add nothing; leaving it out
//     means a routing bug cannot turn into another organization's id appearing
//     in someone's browser.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// channelPrefix namespaces this package's pub/sub channels inside the Redis
// database it shares with sessions and rate limits (internal/auth). Versioned,
// so a future change to the envelope can run alongside the old one during a
// rolling deploy instead of being decoded by an instance that predates it.
const channelPrefix = "collabboard.rt.v1"

// Room is one fan-out group: everybody watching one board in one tenant.
//
// It is a comparable struct so it can be a map key directly, and both fields
// are required — a zero field would collapse distinct rooms onto one key.
type Room struct {
	// TenantID comes from the authenticated principal, never from a client.
	TenantID uuid.UUID

	// BoardID comes from the client and is therefore authorized before a
	// subscription is created. See [Authorizer].
	BoardID uuid.UUID
}

// Channel is the Redis pub/sub channel this room fans out on.
//
// One channel per room, rather than one per tenant or one global channel with
// filtering. An instance serving nothing on a board never issues SUBSCRIBE for
// it, so it never receives that board's traffic at all — which is both the
// bandwidth argument and the isolation one.
func (r Room) Channel() string {
	return fmt.Sprintf("%s.%s.%s", channelPrefix, r.TenantID, r.BoardID)
}

// String renders a room for logs. Deliberately not the channel name, so a log
// line and a Redis channel are not mistaken for each other.
func (r Room) String() string {
	return r.TenantID.String() + "/" + r.BoardID.String()
}

// valid reports whether both halves are set. A room with a zero half is a
// wiring bug: uuid.Nil is a syntactically valid id that matches nothing, so
// letting one through would create a room nobody can ever be authorized for
// and quietly deliver nothing — the same failure mode store.ErrNoTenant exists
// to prevent.
func (r Room) valid() bool {
	return r.TenantID != uuid.Nil && r.BoardID != uuid.Nil
}

// Event is one thing that happened on a board.
//
// Payload is deliberately opaque: this package fans events out and does not
// interpret them, so the shape of a card move belongs to whatever publishes it
// rather than here.
type Event struct {
	// ID is unique per event, so a client can deduplicate across a reconnect.
	ID uuid.UUID `json:"id"`

	// Type names what happened, e.g. "card.moved".
	Type string `json:"type"`

	// ActorID is the user who caused it, taken from the authenticated
	// principal of the request that published it — never from the body.
	ActorID uuid.UUID `json:"actor_id"`

	// OccurredAt is server time at publish, in UTC.
	OccurredAt time.Time `json:"occurred_at"`

	// Payload is event-specific data, passed through untouched.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Frame types sent by the server. A closed set: a client should be able to
// switch on this exhaustively.
const (
	// FrameEvent carries an [Event] for a board the client is subscribed to.
	FrameEvent = "event"

	// FrameSubscribed acknowledges a subscribe. A client should re-fetch the
	// board when it sees this, because events before it were not delivered.
	FrameSubscribed = "subscribed"

	// FrameUnsubscribed acknowledges an unsubscribe, or reports one the server
	// performed on its own — see Reason.
	FrameUnsubscribed = "unsubscribed"

	// FrameError reports a refused or malformed request. It does not close the
	// connection.
	FrameError = "error"

	// FrameShutdown warns that this instance is going away. See [Hub.Shutdown].
	FrameShutdown = "shutdown"

	// FramePong answers a client "ping" frame.
	//
	// Protocol-level ping/pong is what actually keeps the connection alive and
	// reaps dead ones, and it is entirely server-driven — the browser
	// WebSocket API exposes no way to send a ping. This exists so a browser
	// client can still measure round-trip time and notice a half-open
	// connection between two server pings.
	FramePong = "pong"
)

// Reasons carried by an unsubscribed or error frame. Also a closed set,
// because a client branches on them and they end up in logs.
const (
	// ReasonForbidden means the subject may not watch that board — either it
	// is not in their organization, or their membership no longer exists.
	ReasonForbidden = "forbidden"

	// ReasonInvalid means the frame was not something this server understands:
	// an unknown type, or a board id that is not a uuid.
	ReasonInvalid = "invalid_request"

	// ReasonTooManyRooms means the connection is already watching as many
	// boards as it may.
	ReasonTooManyRooms = "too_many_subscriptions"

	// ReasonUnavailable means the server could not complete the request for a
	// reason that is not the client's fault. Retryable.
	ReasonUnavailable = "unavailable"
)

// Frame is one server-to-client message.
//
// One envelope type for every frame rather than a type per message: a client
// decodes once, switches on Type, and cannot be surprised by a shape it has no
// branch for. Every field but Type is omitempty, so the common case — an event
// — costs three fields on the wire.
type Frame struct {
	// Type is one of the Frame* constants.
	Type string `json:"type"`

	// BoardID is set on everything board-scoped. There is no tenant field, by
	// design — see the file comment.
	BoardID *uuid.UUID `json:"board_id,omitempty"`

	// Event is set on FrameEvent.
	Event *Event `json:"event,omitempty"`

	// Reason is set on FrameError and on a server-initiated
	// FrameUnsubscribed. One of the Reason* constants.
	Reason string `json:"reason,omitempty"`

	// Message is human-readable detail for a developer reading a console. It
	// never contains anything derived from stored state.
	Message string `json:"message,omitempty"`

	// ReconnectAfterMs is set on FrameShutdown: how long the client should wait
	// before reconnecting. Jittered per connection so a restart does not
	// produce a synchronised reconnect storm.
	ReconnectAfterMs int64 `json:"reconnect_after_ms,omitempty"`
}

// encode renders a frame. The error is impossible for these types — every
// field is a uuid, a string, an int or a json.RawMessage the caller already
// produced — but returning it beats a silent empty frame if that ever changes.
func (f Frame) encode() ([]byte, error) {
	encoded, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encoding %s frame: %w", f.Type, err)
	}

	return encoded, nil
}

// clientFrame is one client-to-server message.
//
// Three fields, and none of them is a tenant. There is no field a client could
// populate to change which organization it is acting in; that is fixed by the
// token at upgrade time and is not re-read afterwards.
type clientFrame struct {
	// Type is "subscribe", "unsubscribe" or "ping".
	Type string `json:"type"`

	// BoardID is required by subscribe and unsubscribe. It is a string rather
	// than a uuid.UUID so that a malformed value is a frame this package
	// refuses with a reason, not a decode error that kills the connection.
	BoardID string `json:"board_id"`
}

// Client frame types.
const (
	clientSubscribe   = "subscribe"
	clientUnsubscribe = "unsubscribe"
	clientPing        = "ping"
)

// transportEnvelope is what one instance publishes and every instance decodes.
//
// Payload is the already-encoded client [Frame], carried as raw JSON: the
// publishing instance renders it once, and receiving instances hand the same
// bytes to every local connection without decoding or re-encoding. Fan-out is
// therefore a channel send per connection and no allocation per recipient.
type transportEnvelope struct {
	// TenantID and BoardID repeat the room, for the check described in the
	// file comment.
	TenantID uuid.UUID `json:"tenant_id"`
	BoardID  uuid.UUID `json:"board_id"`

	// Payload is the client frame, verbatim.
	Payload json.RawMessage `json:"payload"`
}

func (e transportEnvelope) room() Room {
	return Room{TenantID: e.TenantID, BoardID: e.BoardID}
}
