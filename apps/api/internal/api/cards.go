package api

// Cards, and the move operation the realtime layer exists to broadcast.
//
// # Why the API never mentions a position
//
// A card's rank is a `numeric` the server allocates, and it is not in any
// response. Two reasons, and the first is the important one.
//
// A client that could send a position would be sending a claim about a list it
// last saw. Two clients holding the same slightly-stale list send two positions
// that disagree about where "third from the top" is, and the server has no way
// to tell a deliberate placement from a stale one. `after_card_id` is a claim
// about a single row, which the database can still evaluate after someone else
// has reordered everything around it — and if that row is no longer in the
// target column, the move is refused instead of silently landing somewhere else.
//
// The second: the rank is renumbered from time to time (see
// RebalanceColumnCards). A client holding a rank it read a minute ago would be
// holding a number that no longer means what it meant. Never publishing it means
// no client can come to depend on it.
//
// The cost is that a client cannot compute "insert at index 4" without the list;
// it has the list, because it just rendered the board.
//
// See docs/adr/0004-card-ordering.md.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

type cardBody struct {
	ID          string `json:"id"`
	BoardID     string `json:"board_id"`
	ColumnID    string `json:"column_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type createCardRequest struct {
	Title       string `json:"title" binding:"required"`
	Description string `json:"description"`
}

type patchCardRequest struct {
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// moveCardRequest is the whole move: which column, and which card to sit behind.
// A null or absent after_card_id means "first in that column".
type moveCardRequest struct {
	ColumnID    string  `json:"column_id" binding:"required"`
	AfterCardID *string `json:"after_card_id"`
}

func newCardBody(card store.Card) cardBody {
	return cardBody{
		ID:          card.ID.String(),
		BoardID:     card.BoardID.String(),
		ColumnID:    card.ColumnID.String(),
		Title:       card.Title,
		Description: card.Description,
		CreatedAt:   timestamp(card.CreatedAt),
		UpdatedAt:   timestamp(card.UpdatedAt),
	}
}

// createCardHandler appends a card to a column.
//
// LockColumn is the 404 for a column this tenant cannot see *and* the serialiser
// for the "one past the current maximum" read inside CreateCard.
func createCardHandler(logger *slog.Logger, tenantStore TenantStore, publisher EventPublisher) gin.HandlerFunc {
	return func(c *gin.Context) {
		columnID, ok := pathUUID(c, "column_id")
		if !ok {
			return
		}

		var req createCardRequest
		if !bindJSON(c, &req) {
			return
		}

		title, ok := requiredText(c, "title", req.Title, maxNameLength)
		if !ok {
			return
		}

		description, ok := boundedText(c, "description", req.Description, maxDescriptionLength)
		if !ok {
			return
		}

		card, ok := tenantScopedPublish(c, logger, tenantStore, publisher, "card.create.failed",
			func(ctx context.Context, q store.Querier) (store.Card, error) {
				column, err := q.LockColumn(ctx, columnID)
				if _, err := asNotFound(subjectColumn, column, err); err != nil {
					return store.Card{}, err
				}

				card, err := q.CreateCard(ctx, store.CreateCardParams{
					ColumnID:    columnID,
					Title:       title,
					Description: description,
				})

				return asNotFound(subjectColumn, card, err)
			},
			// Appended to the end of the column, which is why there is no anchor
			// in the payload — see events.go.
			func(card store.Card) BoardEvent {
				return BoardEvent{
					BoardID: card.BoardID,
					Type:    eventCardCreated,
					Payload: cardEventPayload{Card: newCardBody(card)},
				}
			})
		if !ok {
			return
		}

		c.JSON(http.StatusCreated, gin.H{subjectCard: newCardBody(card)})
	}
}

func listCardsByColumnHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		columnID, ok := pathUUID(c, "column_id")
		if !ok {
			return
		}

		cards, ok := tenantScoped(c, logger, tenantStore, "card.list.failed",
			func(ctx context.Context, q store.Querier) ([]store.Card, error) {
				return q.ListCardsByColumn(ctx, columnID)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{"cards": cardBodies(cards)})
	}
}

// listCardsByBoardHandler is the board view's one round trip: every card on the
// board, ordered by column and then by rank, for the client to group.
func listCardsByBoardHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		boardID, ok := pathUUID(c, "board_id")
		if !ok {
			return
		}

		cards, ok := tenantScoped(c, logger, tenantStore, "card.list.failed",
			func(ctx context.Context, q store.Querier) ([]store.Card, error) {
				return q.ListCardsByBoard(ctx, boardID)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{"cards": cardBodies(cards)})
	}
}

func getCardHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		cardID, ok := pathUUID(c, "card_id")
		if !ok {
			return
		}

		card, ok := tenantScoped(c, logger, tenantStore, "card.get.failed",
			func(ctx context.Context, q store.Querier) (store.Card, error) {
				card, err := q.GetCard(ctx, cardID)

				return asNotFound(subjectCard, card, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectCard: newCardBody(card)})
	}
}

func patchCardHandler(logger *slog.Logger, tenantStore TenantStore, publisher EventPublisher) gin.HandlerFunc {
	return func(c *gin.Context) {
		cardID, ok := pathUUID(c, "card_id")
		if !ok {
			return
		}

		var req patchCardRequest
		if !bindJSON(c, &req) {
			return
		}

		title, ok := optionalText(c, "title", req.Title, maxNameLength, false)
		if !ok {
			return
		}

		description, ok := optionalText(c, "description", req.Description, maxDescriptionLength, true)
		if !ok {
			return
		}

		if title == nil && description == nil {
			c.AbortWithStatusJSON(http.StatusBadRequest,
				errorResponse{Error: "at least one of title or description is required"})

			return
		}

		card, ok := tenantScopedPublish(c, logger, tenantStore, publisher, "card.update.failed",
			func(ctx context.Context, q store.Querier) (store.Card, error) {
				card, err := q.UpdateCard(ctx, store.UpdateCardParams{
					CardID:      cardID,
					Title:       title,
					Description: description,
				})

				return asNotFound(subjectCard, card, err)
			},
			// The whole card rather than the fields that changed: a PATCH that
			// only set the title still produces a card body a client can replace
			// wholesale, so there is no merge for a client to get wrong.
			func(card store.Card) BoardEvent {
				return BoardEvent{
					BoardID: card.BoardID,
					Type:    eventCardUpdated,
					Payload: cardEventPayload{Card: newCardBody(card)},
				}
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectCard: newCardBody(card)})
	}
}

// moveCardHandler is the headline operation.
//
// Both ids in the request — the target column and the anchor card — are
// resolved inside the caller's own tenant context, so neither can name anything
// belonging to another organization. Neither is checked against an owner: a
// foreign column simply does not exist to this transaction.
func moveCardHandler(logger *slog.Logger, tenantStore TenantStore, publisher EventPublisher) gin.HandlerFunc {
	return func(c *gin.Context) {
		cardID, ok := pathUUID(c, "card_id")
		if !ok {
			return
		}

		var req moveCardRequest
		if !bindJSON(c, &req) {
			return
		}

		columnID, err := uuid.Parse(req.ColumnID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: "column_id must be a uuid"})

			return
		}

		afterCardID, ok := optionalUUID(c, "after_card_id", req.AfterCardID)
		if !ok {
			return
		}

		moved, ok := tenantScopedPublish(c, logger, tenantStore, publisher, "card.move.failed",
			func(ctx context.Context, q store.Querier) (movedCard, error) {
				return moveCard(ctx, q, cardID, columnID, afterCardID)
			},
			// The anchor is echoed rather than re-derived. MoveCard placed the
			// card strictly between it and the next sibling, so it was the
			// card's predecessor when the rank was computed — see
			// cardMovedPayload for why that is not the same as a promise about
			// the present. A rebalance may have renumbered the column
			// afterwards, which changes every rank and no order, and clients
			// never see a rank, so it needs no event of its own.
			func(moved movedCard) BoardEvent {
				return BoardEvent{
					BoardID: moved.card.BoardID,
					Type:    eventCardMoved,
					Payload: cardMovedPayload{
						Card:         newCardBody(moved.card),
						FromColumnID: moved.fromColumnID.String(),
						AfterCardID:  optionalID(afterCardID),
					},
				}
			})
		if !ok {
			return
		}

		// After the commit, and after the broadcast, so the line is only ever
		// written about a move that happened. The refused half of this is in
		// crud.go's logRefusal — see movelog.go for what is deliberately absent
		// from both.
		logDomainEvent(c, logger, slog.LevelInfo, "card moved", logEventCardMoved,
			cardMoveAttrs(moved.card.ID, moved.card.BoardID, moved.fromColumnID, moved.card.ColumnID, afterCardID)...)

		if moved.rebalanced {
			logDomainEvent(c, logger, slog.LevelInfo, "renumbered a column's cards", logEventCardOrderRebalanced,
				slog.String("board_id", moved.card.BoardID.String()),
				slog.String("column_id", moved.card.ColumnID.String()),
			)
		}

		c.JSON(http.StatusOK, gin.H{subjectCard: newCardBody(moved.card)})
	}
}

// moveCard is the body of the move, inside the tenant transaction.
//
// The order of the three statements is the concurrency design:
//
//  1. lock the destination column, which serialises every other move or create
//     targeting it — so the midpoint is computed against an order nobody else is
//     changing, and two cards can never be handed the same rank;
//  2. read the card, which is the 404 and the source of the board check;
//  3. update the card, which additionally takes the card's own row lock — so two
//     clients moving the *same* card serialise there, and the second one
//     recomputes its midpoint against the order the first one left behind.
//
// Only one lock is ever waited for while another is held, so there is no cycle
// and no deadlock between two concurrent moves.
func moveCard(ctx context.Context, q store.Querier, cardID, columnID uuid.UUID, afterCardID *uuid.UUID) (movedCard, error) {
	column, err := q.LockColumn(ctx, columnID)

	target, err := asNotFound(subjectColumn, column, err)
	if err != nil {
		return movedCard{}, err
	}

	card, err := q.GetCard(ctx, cardID)

	current, err := asNotFound(subjectCard, card, err)
	if err != nil {
		return movedCard{}, err
	}

	// Cards do not change board. The composite foreign key would reject it
	// anyway; checking here is what turns that into a 409 with a sentence rather
	// than a 500 with a constraint name in the log.
	if current.BoardID != target.BoardID {
		return movedCard{}, conflict("column_id names a column on a different board").
			loggedAs(logEventCardMoveRefused, append(
				cardMoveAttrs(cardID, current.BoardID, current.ColumnID, columnID, afterCardID),
				slog.String("reason", reasonCrossBoardColumn),
				// The only refusal where the two boards differ, so the target's
				// board is worth naming: without it the line says a column was
				// on "a different board" without saying which.
				slog.String("to_board_id", target.BoardID.String()),
			)...)
	}

	moved, err := q.MoveCard(ctx, store.MoveCardParams{
		CardID:      cardID,
		ColumnID:    columnID,
		AfterCardID: afterCardID,
	})

	if errors.Is(err, store.ErrNoRows) {
		// The card exists, the column exists, and they share a board — so the
		// only remaining reason for no row is the anchor. A client that dragged
		// past a card someone else has just moved or deleted gets this.
		//
		// The anchor is on the refusal's log line, because "which anchor was
		// stale" is the whole question a refused drag raises and the status code
		// alone cannot answer it.
		return movedCard{}, conflict("after_card_id is not a card in that column").
			loggedAs(logEventCardMoveRefused, append(
				cardMoveAttrs(cardID, current.BoardID, current.ColumnID, columnID, afterCardID),
				slog.String("reason", reasonStaleCardAnchor),
			)...)
	}

	if err != nil {
		return movedCard{}, err
	}

	if moved.NeedsRebalance {
		// Still under the column lock. Renumbering is the only statement that
		// writes every row in a column, and it must not run while another move
		// is comparing against a rank it is about to rewrite.
		if err := q.RebalanceColumnCards(ctx, columnID); err != nil {
			return movedCard{}, err
		}
	}

	return movedCard{
		// Reported rather than logged here: this is inside the transaction, and
		// a renumbering that is about to roll back did not happen. The handler
		// logs it once the commit has returned.
		rebalanced: moved.NeedsRebalance,
		card: store.Card{
			ID:          moved.ID,
			TenantID:    moved.TenantID,
			BoardID:     moved.BoardID,
			ColumnID:    moved.ColumnID,
			Title:       moved.Title,
			Description: moved.Description,
			Position:    moved.Position,
			AssigneeID:  moved.AssigneeID,
			DueAt:       moved.DueAt,
			CreatedAt:   moved.CreatedAt,
			UpdatedAt:   moved.UpdatedAt,
		},
		// Read inside the same transaction, before the update. It is the only
		// place the card's previous column still exists, and a client keeping
		// one list per column needs it to know which list to take the card out
		// of.
		fromColumnID: current.ColumnID,
	}, nil
}

// movedCard is a move's result: the card as it now is, the column it came from,
// and whether the move renumbered the column. The second half exists only for
// the event — see events.go — and the third only for the log.
type movedCard struct {
	card         store.Card
	fromColumnID uuid.UUID

	// rebalanced is true when this move drove RebalanceColumnCards. ADR 0004
	// set the threshold low on purpose "so that the renumbering path runs often
	// enough to be a tested path rather than a theoretical one", and until this
	// field existed there was no way to tell whether it ever did.
	//
	// It is not on the realtime event: renumbering changes every rank and no
	// order, and a client never sees a rank, so there is nothing for one to do
	// about it.
	rebalanced bool
}

// deleteCardHandler removes a card.
//
// DeleteCard returns the row it deleted rather than a count, because after the
// statement runs the card's board is the one thing nothing else knows and the
// event has to be addressed to it. "No row" is still the 404, and still means
// the same thing for a card that never existed and for one belonging to another
// organization.
func deleteCardHandler(logger *slog.Logger, tenantStore TenantStore, publisher EventPublisher) gin.HandlerFunc {
	return func(c *gin.Context) {
		cardID, ok := pathUUID(c, "card_id")
		if !ok {
			return
		}

		_, ok = tenantScopedPublish(c, logger, tenantStore, publisher, "card.delete.failed",
			func(ctx context.Context, q store.Querier) (store.Card, error) {
				card, err := q.DeleteCard(ctx, cardID)

				return asNotFound(subjectCard, card, err)
			},
			func(card store.Card) BoardEvent {
				return BoardEvent{
					BoardID: card.BoardID,
					Type:    eventCardDeleted,
					Payload: cardDeletedPayload{
						CardID:   card.ID.String(),
						ColumnID: card.ColumnID.String(),
					},
				}
			})
		if !ok {
			return
		}

		c.Status(http.StatusNoContent)
	}
}

func cardBodies(cards []store.Card) []cardBody {
	bodies := make([]cardBody, 0, len(cards))
	for _, card := range cards {
		bodies = append(bodies, newCardBody(card))
	}

	return bodies
}
