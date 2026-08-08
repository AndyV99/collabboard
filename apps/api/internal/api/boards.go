package api

// Boards, which belong to a project.
//
// A board id appears in the path here and in the WebSocket routes, which is fine
// and is not what auth_bola_test.go forbids: a board is an object inside a
// tenant, resolved against the caller's own tenant context. An *organization* in
// the path is what stays impossible.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

type boardBody struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type createBoardRequest struct {
	Name string `json:"name" binding:"required"`
}

type patchBoardRequest struct {
	Name *string `json:"name"`
}

func newBoardBody(board store.Board) boardBody {
	return boardBody{
		ID:        board.ID.String(),
		ProjectID: board.ProjectID.String(),
		Name:      board.Name,
		CreatedAt: timestamp(board.CreatedAt),
		UpdatedAt: timestamp(board.UpdatedAt),
	}
}

// createBoardHandler creates a board inside a project.
//
// The project id comes from the path and is never checked for ownership here:
// CreateBoard is an INSERT ... SELECT over projects, so a project belonging to
// another tenant is filtered out by the policy and the insert produces no row.
// 404, not 403 — see notFound.
func createBoardHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := pathUUID(c, "project_id")
		if !ok {
			return
		}

		var req createBoardRequest
		if !bindJSON(c, &req) {
			return
		}

		name, ok := requiredText(c, "name", req.Name, maxNameLength)
		if !ok {
			return
		}

		board, ok := tenantScoped(c, logger, tenantStore, "board.create.failed",
			func(ctx context.Context, q store.Querier) (store.Board, error) {
				board, err := q.CreateBoard(ctx, store.CreateBoardParams{
					ProjectID: projectID,
					Name:      name,
				})

				return asNotFound(subjectProject, board, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusCreated, gin.H{subjectBoard: newBoardBody(board)})
	}
}

// listBoardsHandler lists a project's boards.
//
// An empty list is the answer both for a project with no boards and for a
// project id belonging to another tenant. Reading the project first to
// distinguish them would turn this into an existence oracle for one extra round
// trip, so it does not.
func listBoardsHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := pathUUID(c, "project_id")
		if !ok {
			return
		}

		boards, ok := tenantScoped(c, logger, tenantStore, "board.list.failed",
			func(ctx context.Context, q store.Querier) ([]store.Board, error) {
				return q.ListBoardsByProject(ctx, projectID)
			})
		if !ok {
			return
		}

		bodies := make([]boardBody, 0, len(boards))
		for _, board := range boards {
			bodies = append(bodies, newBoardBody(board))
		}

		c.JSON(http.StatusOK, gin.H{"boards": bodies})
	}
}

func getBoardHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		boardID, ok := pathUUID(c, "board_id")
		if !ok {
			return
		}

		board, ok := tenantScoped(c, logger, tenantStore, "board.get.failed",
			func(ctx context.Context, q store.Querier) (store.Board, error) {
				board, err := q.GetBoard(ctx, boardID)

				return asNotFound(subjectBoard, board, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectBoard: newBoardBody(board)})
	}
}

func patchBoardHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		boardID, ok := pathUUID(c, "board_id")
		if !ok {
			return
		}

		var req patchBoardRequest
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

		board, ok := tenantScoped(c, logger, tenantStore, "board.update.failed",
			func(ctx context.Context, q store.Querier) (store.Board, error) {
				board, err := q.UpdateBoard(ctx, store.UpdateBoardParams{
					BoardID: boardID,
					Name:    name,
				})

				return asNotFound(subjectBoard, board, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectBoard: newBoardBody(board)})
	}
}

// deleteBoardHandler removes a board and, through the composite foreign keys,
// its columns and cards.
//
// 404 rather than 204 for an id that matched nothing. Delete is idempotent in
// the sense that matters — a second call changes nothing — but answering 204 for
// an unknown id would also answer 204 for another tenant's board, which reads
// like a successful cross-tenant delete to anyone testing this from outside.
func deleteBoardHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		boardID, ok := pathUUID(c, "board_id")
		if !ok {
			return
		}

		_, ok = tenantScoped(c, logger, tenantStore, "board.delete.failed",
			func(ctx context.Context, q store.Querier) (struct{}, error) {
				rows, err := q.DeleteBoard(ctx, boardID)
				if err != nil {
					return struct{}{}, err
				}

				if rows == 0 {
					return struct{}{}, notFound(subjectBoard)
				}

				return struct{}{}, nil
			})
		if !ok {
			return
		}

		c.Status(http.StatusNoContent)
	}
}
