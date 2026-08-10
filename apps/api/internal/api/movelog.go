package api

// The domain log lines for a card or column move.
//
// # Why a move gets a line of its own
//
// The request log in middleware.go writes `path` as c.FullPath() — the route
// *template*. Every move therefore logs as POST /api/v1/cards/:card_id/move
// with a status and a duration, and eighteen consecutive moves are eighteen
// identical lines. That is the one write in this service whose ordering is
// hardest to reconstruct afterwards (ADR 0004 turns on last-writer-wins per
// card and on a parent-row lock), and it was the least traceable.
//
// So each move emits one line naming its subject: which card, on which board,
// out of which column, into which column, behind which anchor. Two moves are
// distinguishable, and a sequence of them replays the order the server chose.
//
// # What is deliberately not in these lines
//
// **The rank.** ADR 0004 makes the rank server-side only: it is in no response,
// and cards.go's file comment explains that a client which could see one would
// come to depend on a number that RebalanceColumnCards rewrites. A log is a
// lower bar than an API response — but these lines are JSON on stdout, which in
// a deployed environment means a third-party aggregator, and "the ordering
// scheme's internals are not published" is worth keeping true in both places
// rather than only in the one anybody checks. It also buys nothing: the anchor
// is what the mover asked for and what a replay needs, and a bare `numeric`
// midpoint tells a reader nothing the sequence of anchors does not. The
// rebalance lines below cover the one question a rank would have answered —
// whether the renumbering path ran — without publishing the numbers.
//
// **Titles and names.** They are user content and may be confidential; the ids
// are what reconstructs an order. Same reasoning as the event payloads.
//
// # These are logs, not metrics
//
// Ids are fine here because a log line is not a time series. Nothing per-move
// goes on a Prometheus label — RED/USE instrumentation is #12 and gets to make
// its own cardinality decisions rather than inheriting one made here.

import (
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// The move log vocabulary.
//
// `subject.past-tense-verb` for something that happened and
// `subject.verb.refused` for something the server declined — the dotted form,
// matching realtime.subscription.refused and realtime.upgrade.refused.
// (internal/auth's auth.member.add_refused runs the last two words together and
// is the outlier; the dotted form is the one to follow, because it keeps the
// refusal greppable as a suffix across subjects.) These names are what
// everything later gets grepped by, so they are constants rather than literals
// at the call site.
//
// The two "moved" names are spelled the same as the realtime event types in
// events.go, and are deliberately *separate* constants. The wire contract is
// versioned by realtime.channelPrefix and the frontend switches on it; the log
// vocabulary is read by whoever is answering a question in an aggregator. They
// agree today because agreeing is convenient, not because one defines the
// other, and a version bump on the envelope must not silently rename a log
// field somebody's saved search depends on.
const (
	logEventCardMoved           = "card.moved"
	logEventCardMoveRefused     = "card.move.refused"
	logEventCardOrderRebalanced = "card.order.rebalanced"

	logEventColumnMoved           = "column.moved"
	logEventColumnMoveRefused     = "column.move.refused"
	logEventColumnOrderRebalanced = "column.order.rebalanced"
)

// Why a move was refused, as a token rather than a sentence.
//
// The prose message is on the line too (as `detail`), because it is what the
// caller was told and the two should be comparable. But prose is what gets
// reworded, and a saved search should survive a copy edit.
const (
	reasonStaleCardAnchor   = "stale_after_card_id"
	reasonStaleColumnAnchor = "stale_after_column_id"
	reasonCrossBoardColumn  = "column_on_another_board"
)

// requestIDFrom returns the id requestLogger assigned this request, so a domain
// line joins to the request line that bracketed it.
//
// Empty only if this ran outside the middleware, which in this package means a
// test that mounted a handler on a bare engine.
func requestIDFrom(c *gin.Context) string {
	return c.GetString(requestIDHeader)
}

// requestAttrs are the fields every domain line carries: which request, whose
// tenant, and which member did it.
//
// The tenant and the actor come from [principalFrom] — the verified claim — for
// the same reason [publishBoardEvent] fills them in itself rather than letting a
// handler pass them. A log line naming the wrong actor is worse than one naming
// none.
//
// `actor_user_id` rather than `actor_id` or `user_id`, because this service
// already made that choice: internal/auth/members.go names the member who
// performed a write `actor_user_id` and reserves `user_id` for the member a
// write is *about*. A move has no second user, so `user_id` would read fine
// here and then collide the day a card gains an assignee — see #48. (The
// realtime envelope's `actor_id` is a third spelling, but that is JSON on a
// websocket rather than a log key, and the two vocabularies are separate for
// the reason the event-name comment above gives.)
func requestAttrs(c *gin.Context) []slog.Attr {
	attrs := make([]slog.Attr, 0, 3)
	attrs = append(attrs, slog.String("request_id", requestIDFrom(c)))

	if principal, ok := principalFrom(c); ok {
		attrs = append(attrs,
			slog.String("tenant_id", principal.TenantID.String()),
			slog.String("actor_user_id", principal.UserID.String()),
		)
	}

	return attrs
}

// logDomainEvent writes one line about something the domain did, in the shape
// the rest of the service uses: a human sentence as the message, a dotted
// `event` name to search on, and snake_case context fields.
func logDomainEvent(
	c *gin.Context,
	logger *slog.Logger,
	level slog.Level,
	message, event string,
	attrs ...slog.Attr,
) {
	all := make([]slog.Attr, 0, 4+len(attrs))
	all = append(all, slog.String("event", event))
	all = append(all, requestAttrs(c)...)
	all = append(all, attrs...)

	logger.LogAttrs(c.Request.Context(), level, message, all...)
}

// How a column is named on these lines, since three of them mention one.
//
// `column_id` is the column a line is *about* — the one that moved, or the one
// whose cards were renumbered. A card move is about a card and touches two
// columns, so it names neither bare: it uses `from_column_id` and
// `to_column_id`, and a search for `column_id=X` deliberately does not match it.
// Finding everything that happened to one column therefore means asking for
// `column_id=X OR to_column_id=X OR from_column_id=X`, which is worse than one
// term and better than a term that silently means two things.

// cardMoveAttrs describes a card move: where it went, and what it was placed
// behind.
//
// One builder for both the success line and the refusal line on purpose. The
// question a refusal raises is "what did this user ask for, and how does it
// compare to the moves around it" — which only works if the two lines describe a
// move with the same field names. Two construction sites would be free to drift.
//
// afterCardID is rendered as an explicit JSON null for "first in the column",
// never omitted: absent and null mean different things here, exactly as they do
// in cardMovedPayload.
func cardMoveAttrs(cardID, boardID, fromColumnID, toColumnID uuid.UUID, afterCardID *uuid.UUID) []slog.Attr {
	return []slog.Attr{
		slog.String("card_id", cardID.String()),
		slog.String("board_id", boardID.String()),
		slog.String("from_column_id", fromColumnID.String()),
		slog.String("to_column_id", toColumnID.String()),
		slog.Any("after_card_id", optionalID(afterCardID)),
	}
}

// columnMoveAttrs describes a column move. There is no from/to pair: a column
// does not change board, so the only thing a move changes is its neighbour.
func columnMoveAttrs(columnID, boardID uuid.UUID, afterColumnID *uuid.UUID) []slog.Attr {
	return []slog.Attr{
		slog.String("column_id", columnID.String()),
		slog.String("board_id", boardID.String()),
		slog.Any("after_column_id", optionalID(afterColumnID)),
	}
}
