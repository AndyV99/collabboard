package api

// Columns, which belong to a board and order the cards inside them.
//
// Reordering is neighbour-relative for the same reason card moves are — see
// cards.go and docs/adr/0004-card-ordering.md.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

type columnBody struct {
	ID        string `json:"id"`
	BoardID   string `json:"board_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type createColumnRequest struct {
	Name string `json:"name" binding:"required"`
}

type patchColumnRequest struct {
	Name *string `json:"name"`
}

// moveColumnRequest places a column immediately after another one. A null or
// absent after_column_id means "make this the first column".
type moveColumnRequest struct {
	AfterColumnID *string `json:"after_column_id"`
}

func newColumnBody(column store.Column) columnBody {
	return columnBody{
		ID:        column.ID.String(),
		BoardID:   column.BoardID.String(),
		Name:      column.Name,
		CreatedAt: timestamp(column.CreatedAt),
		UpdatedAt: timestamp(column.UpdatedAt),
	}
}

// createColumnHandler appends a column to a board.
//
// LockBoard runs first and does two jobs: it is the 404 for a board this tenant
// cannot see, and it serialises the "one past the current maximum" read inside
// CreateColumn against a concurrent create, so two columns cannot claim the same
// position.
func createColumnHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		boardID, ok := pathUUID(c, "board_id")
		if !ok {
			return
		}

		var req createColumnRequest
		if !bindJSON(c, &req) {
			return
		}

		name, ok := requiredText(c, "name", req.Name, maxNameLength)
		if !ok {
			return
		}

		column, ok := tenantScoped(c, logger, tenantStore, "column.create.failed",
			func(ctx context.Context, q store.Querier) (store.Column, error) {
				board, err := q.LockBoard(ctx, boardID)
				if _, err := asNotFound(subjectBoard, board, err); err != nil {
					return store.Column{}, err
				}

				column, err := q.CreateColumn(ctx, store.CreateColumnParams{
					BoardID: boardID,
					Name:    name,
				})

				return asNotFound(subjectBoard, column, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusCreated, gin.H{subjectColumn: newColumnBody(column)})
	}
}

func listColumnsHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		boardID, ok := pathUUID(c, "board_id")
		if !ok {
			return
		}

		columns, ok := tenantScoped(c, logger, tenantStore, "column.list.failed",
			func(ctx context.Context, q store.Querier) ([]store.Column, error) {
				return q.ListColumnsByBoard(ctx, boardID)
			})
		if !ok {
			return
		}

		bodies := make([]columnBody, 0, len(columns))
		for _, column := range columns {
			bodies = append(bodies, newColumnBody(column))
		}

		c.JSON(http.StatusOK, gin.H{"columns": bodies})
	}
}

func patchColumnHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		columnID, ok := pathUUID(c, "column_id")
		if !ok {
			return
		}

		var req patchColumnRequest
		if !bindJSON(c, &req) {
			return
		}

		if req.Name == nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: "name is required"})

			return
		}

		name, ok := requiredText(c, "name", *req.Name, maxNameLength)
		if !ok {
			return
		}

		column, ok := tenantScoped(c, logger, tenantStore, "column.update.failed",
			func(ctx context.Context, q store.Querier) (store.Column, error) {
				column, err := q.UpdateColumn(ctx, store.UpdateColumnParams{
					ColumnID: columnID,
					Name:     name,
				})

				return asNotFound(subjectColumn, column, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectColumn: newColumnBody(column)})
	}
}

// moveColumnHandler reorders a column within its board.
//
// The board is not a parameter: it is read from the column, so there is no way
// to ask for a column to be moved onto a board it does not belong to.
func moveColumnHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		columnID, ok := pathUUID(c, "column_id")
		if !ok {
			return
		}

		var req moveColumnRequest
		if !bindJSON(c, &req) {
			return
		}

		afterColumnID, ok := optionalUUID(c, "after_column_id", req.AfterColumnID)
		if !ok {
			return
		}

		column, ok := tenantScoped(c, logger, tenantStore, "column.move.failed",
			func(ctx context.Context, q store.Querier) (store.Column, error) {
				return moveColumn(ctx, q, columnID, afterColumnID)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectColumn: newColumnBody(column)})
	}
}

// moveColumn is the body of the move, inside the tenant transaction.
func moveColumn(ctx context.Context, q store.Querier, columnID uuid.UUID, afterColumnID *uuid.UUID) (store.Column, error) {
	found, err := q.GetColumn(ctx, columnID)

	current, err := asNotFound(subjectColumn, found, err)
	if err != nil {
		return store.Column{}, err
	}

	// One lock, on the board, held for the rest of the transaction. Every write
	// that allocates a column position takes it, so the midpoint below is
	// computed against an order nobody else is changing.
	board, err := q.LockBoard(ctx, current.BoardID)
	if _, err := asNotFound(subjectBoard, board, err); err != nil {
		return store.Column{}, err
	}

	moved, err := q.MoveColumn(ctx, store.MoveColumnParams{
		ColumnID:      columnID,
		BoardID:       current.BoardID,
		AfterColumnID: afterColumnID,
	})

	if errors.Is(err, store.ErrNoRows) {
		// The column is known to exist and the board lock is held, so the only
		// remaining reason for no row is the anchor: an after_column_id that is
		// not a sibling. That is what a stale client sends after someone else
		// deleted the column it was dragging past.
		return store.Column{}, conflict("after_column_id is not another column on this board")
	}

	if err != nil {
		return store.Column{}, err
	}

	if moved.NeedsRebalance {
		// Still under the board lock, so this cannot rewrite a position a
		// concurrent move is midway through comparing against.
		if err := q.RebalanceBoardColumns(ctx, current.BoardID); err != nil {
			return store.Column{}, err
		}
	}

	return store.Column{
		ID:        moved.ID,
		TenantID:  moved.TenantID,
		BoardID:   moved.BoardID,
		Name:      moved.Name,
		Position:  moved.Position,
		CreatedAt: moved.CreatedAt,
		UpdatedAt: moved.UpdatedAt,
	}, nil
}

func deleteColumnHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		columnID, ok := pathUUID(c, "column_id")
		if !ok {
			return
		}

		_, ok = tenantScoped(c, logger, tenantStore, "column.delete.failed",
			func(ctx context.Context, q store.Querier) (struct{}, error) {
				rows, err := q.DeleteColumn(ctx, columnID)
				if err != nil {
					return struct{}{}, err
				}

				if rows == 0 {
					return struct{}{}, notFound(subjectColumn)
				}

				return struct{}{}, nil
			})
		if !ok {
			return
		}

		c.Status(http.StatusNoContent)
	}
}
