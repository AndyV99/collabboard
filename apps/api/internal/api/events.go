package api

// The realtime event contract: what a committed write broadcasts, to whom, and
// when.
//
// # The rule this file exists to make structural
//
// > An event is published after the transaction has committed, and never from
// > inside it.
//
// A publish from inside [store.Store.WithTenant]'s callback would announce a
// change that a subsequent rollback un-does, and every connected client would
// then show a card that does not exist until somebody reloads. That is strictly
// worse than not broadcasting at all, because it is wrong rather than merely
// stale.
//
// So the callback cannot publish. It is handed a [store.Querier] and nothing
// else — no publisher is in scope down there — and the description of the event
// is produced by a *second* function that [tenantScopedPublish] calls only after
// WithTenant has returned nil. A rolled-back write publishing nothing is
// therefore a property of the types, not a rule someone has to remember on the
// next handler.
//
// # When the write commits and the publish fails
//
// The write stands and the failure is logged. It is not retried, not buffered
// and not written to an outbox. The reasoning is in
// docs/adr/0005-realtime-event-delivery.md; the short version is that the
// transport this feeds is itself at-most-once, so durability up to Redis would
// buy nothing a client can rely on — every client already re-fetches the board
// when it subscribes, which is the recovery path an outbox would be trying to
// avoid needing and which has to exist anyway.
//
// # Ordering
//
// The publish happens *before* the HTTP response is written, which is what makes
// the useful guarantee true: if a client issues a second write only after the
// first one's response arrived, the two events are published in that order, and
// Redis delivers one total order per board to every instance. Two genuinely
// concurrent writers get an order the server picked — the same order for every
// client, which is the property that matters — see the ADR.
//
// # Authorization
//
// Every field of every event below comes from a row that a tenant-scoped
// transaction returned, so it is already the caller's tenant's data. The room is
// (tenant, board), and a subscriber is only in it if [realtime.StoreAuthorizer]
// resolved that board inside their own tenant. Membership in this schema is per
// organization, so "may watch this board" and "may read this board" are the same
// question, and there is nothing in a payload here that a room member could not
// have fetched over REST.

import (
	"context"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Event types. A closed set, and the names are a wire contract: the frontend
// switches on them, so they are `subject.past-tense-verb` and they do not
// change without a version bump on the envelope (see realtime.channelPrefix).
const (
	eventCardCreated = "card.created"
	eventCardUpdated = "card.updated"
	eventCardMoved   = "card.moved"
	eventCardDeleted = "card.deleted"

	eventColumnCreated = "column.created"
	eventColumnUpdated = "column.updated"
	eventColumnMoved   = "column.moved"
	eventColumnDeleted = "column.deleted"

	eventBoardUpdated = "board.updated"
	eventBoardDeleted = "board.deleted"
)

// publishTimeout bounds one broadcast.
//
// It is short on purpose. The write has already committed and the response is
// waiting behind this call, so an unreachable Redis must cost a card move a
// bounded pause rather than the driver's own timeouts stacked end to end. Two
// seconds is far above the measured cost of a PUBLISH (sub-millisecond, see
// internal/realtime/README.md) and far below anything a user would call a hang.
const publishTimeout = 2 * time.Second

// BoardEvent is one committed change, addressed to the board whose watchers
// should hear about it.
//
// TenantID and ActorID are filled in by [publishBoardEvent] from the
// authenticated principal and are deliberately not settable by a handler: an
// event that could name its own tenant would be a second place the tenant comes
// from, and one that could name its own actor would let any member forge another
// member's action in every open browser on the board.
type BoardEvent struct {
	// TenantID is the room's tenant half. Set from the principal.
	TenantID uuid.UUID

	// ActorID is who caused it. Set from the principal.
	ActorID uuid.UUID

	// BoardID is the board the change happened on. It comes from a row the
	// tenant-scoped transaction returned, never from the request path, so it
	// cannot address a room outside the caller's own tenant.
	BoardID uuid.UUID

	// Type is one of the event constants above. The zero value means "nothing
	// to announce": [publishBoardEvent] returns without publishing, which is how
	// a write that turns out to have nothing worth broadcasting says so.
	Type string

	// Payload is the event-specific body. It is rendered to JSON by the
	// publisher and travels to clients untouched.
	Payload any
}

// EventPublisher fans a committed change out to a board's subscribers.
//
// Declared here and implemented in internal/realtime, for the same reason
// [RealtimeDeps] holds handlers rather than a hub: internal/realtime imports
// this package for PrincipalFromContext, so the dependency cannot run the other
// way. The interface names no realtime type, which is what lets that stay true.
//
// Implementations must not return until the event has been handed to the
// transport, because the response to the write is waiting on it — see the
// ordering note in the file comment.
type EventPublisher interface {
	PublishBoardEvent(ctx context.Context, event BoardEvent) error
}

// The event payload shapes.
//
// Each one carries the *same* representation of the object that the REST
// endpoints return — cardBody, columnBody, boardBody — rather than a parallel
// "event" shape. A client therefore has one Card type and one decoder, and a
// field added to a response cannot silently fail to appear in the event that
// announces it.
//
// Three conventions the frontend can rely on:
//
//   - A `*.created` card or column is **appended** to the end of its parent.
//     That is what CreateCard and CreateColumn do (one past the current
//     maximum), and it is why these payloads carry no anchor.
//   - A `*.moved` payload carries the anchor the mover named, with an explicit
//     `null` for "first". It is not omitted, because absent and null mean
//     different things here and a client must be able to tell them apart. The
//     rank itself is never published — ADR 0004.
//   - A `*.deleted` payload carries ids only. There is nothing left to render,
//     and a client removes by id.
type (
	// cardEventPayload announces a created or updated card.
	cardEventPayload struct {
		Card cardBody `json:"card"`
	}

	// cardMovedPayload announces a move: the card as it now is, and where it
	// landed relative to its new neighbour.
	cardMovedPayload struct {
		Card cardBody `json:"card"`

		// AfterCardID is the card this one now sits behind, or null for first
		// in the column. Applying "put this card after that one" reproduces the
		// server's order without the client knowing any rank.
		AfterCardID *string `json:"after_card_id"`
	}

	// cardDeletedPayload announces a removed card. ColumnID is where it was, so
	// a client does not have to search every column for it.
	cardDeletedPayload struct {
		CardID   string `json:"card_id"`
		ColumnID string `json:"column_id"`
	}

	// columnEventPayload announces a created or updated column.
	columnEventPayload struct {
		Column columnBody `json:"column"`
	}

	// columnMovedPayload announces a reordered column.
	columnMovedPayload struct {
		Column columnBody `json:"column"`

		// AfterColumnID is null for "first on the board".
		AfterColumnID *string `json:"after_column_id"`
	}

	// columnDeletedPayload announces a removed column.
	//
	// Deleting a column deletes its cards through the composite foreign key,
	// and no card.deleted follows: a client drops the column and everything in
	// it. One event for one user action beats n+1 events describing the same
	// one.
	columnDeletedPayload struct {
		ColumnID string `json:"column_id"`
	}

	// boardEventPayload announces a renamed board.
	boardEventPayload struct {
		Board boardBody `json:"board"`
	}

	// boardDeletedPayload announces that the board this room is about is gone.
	//
	// Like a deleted column it is one event rather than a cascade of them, and
	// it is the last event the room will ever carry: a client should stop
	// showing the board rather than try to reconcile it.
	boardDeletedPayload struct {
		BoardID string `json:"board_id"`
	}
)

// optionalID renders a nullable uuid for a payload, keeping JSON null for "no
// anchor, put it first".
func optionalID(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}

	rendered := id.String()

	return &rendered
}

// publishBoardEvent broadcasts one committed change, and never fails a request.
//
// The context is deliberately detached from cancellation. By the time this runs
// the write is committed and durable; a client that hung up mid-request must not
// be the reason every *other* client on the board never hears about it. Values —
// trace ids, the logger's handler context — are kept, so the publish is still
// correlated with the request that caused it.
func publishBoardEvent(c *gin.Context, logger *slog.Logger, publisher EventPublisher, event BoardEvent) {
	if publisher == nil || event.Type == "" {
		return
	}

	principal, ok := principalFrom(c)
	if !ok {
		// Unreachable: tenantScoped refused the write without a principal. A
		// silent return is still better than publishing an event with no actor.
		return
	}

	event.TenantID = principal.TenantID
	event.ActorID = principal.UserID

	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), publishTimeout)
	defer cancel()

	if err := publisher.PublishBoardEvent(ctx, event); err != nil {
		// The write stands. This is the one failure mode ADR 0005 accepts, so
		// it is logged at error level with everything needed to notice a pattern
		// of it — a board whose clients have quietly stopped seeing each other.
		logger.ErrorContext(ctx, "publishing a realtime event failed; the write is committed and clients will not see it until they refetch",
			slog.String("event", "realtime.publish.failed"),
			slog.String("event_type", event.Type),
			slog.String("tenant_id", event.TenantID.String()),
			slog.String("board_id", event.BoardID.String()),
			slog.Any("error", err),
		)
	}
}
