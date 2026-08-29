package api

// Projects: the top of the tenant-scoped hierarchy.
//
// Nothing here consults the request for a tenant. See crud.go.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

type projectBody struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	ArchivedAt  *string `json:"archived_at"`
	CreatedAt   string  `json:"created_at"`
	UpdatedAt   string  `json:"updated_at"`
}

type createProjectRequest struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
}

// patchProjectRequest is a PATCH: a pointer field distinguishes "not mentioned"
// from "set to empty", which a plain string cannot. Renaming a project without
// mentioning its description must not blank the description.
type patchProjectRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func newProjectBody(project store.Project) projectBody {
	return projectBody{
		ID:          project.ID.String(),
		Name:        project.Name,
		Description: project.Description,
		ArchivedAt:  optionalTimestamp(project.ArchivedAt),
		CreatedAt:   timestamp(project.CreatedAt),
		UpdatedAt:   timestamp(project.UpdatedAt),
	}
}

func createProjectHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req createProjectRequest
		if !bindJSON(c, &req) {
			return
		}

		name, ok := requiredText(c, "name", req.Name, maxNameLength)
		if !ok {
			return
		}

		description, ok := boundedText(c, "description", req.Description, maxDescriptionLength)
		if !ok {
			return
		}

		project, ok := tenantScoped(c, logger, tenantStore, "project.create.failed",
			func(ctx context.Context, q store.Querier) (store.Project, error) {
				return q.CreateProject(ctx, store.CreateProjectParams{
					Name:        name,
					Description: description,
				})
			})
		if !ok {
			return
		}

		c.JSON(http.StatusCreated, gin.H{subjectProject: newProjectBody(project)})
	}
}

// listProjectsHandler returns the tenant's projects: active by default,
// archived with `?archived=true`.
//
// Archived ones are excluded from the default rather than flagged, because this
// is the query the board picker is built on and an archived project is exactly
// the one nobody wants in that list. Listing them at all was the gap #49 filed:
// archiving used to be a soft delete with no view and no undo, so a user who
// archived the wrong project had no way back through the product.
//
// A query parameter rather than a second route, because it is the same
// collection under a different filter -- and an unrecognised value is refused
// rather than treated as false, so `?archived=yes` is an error instead of
// silently returning the wrong list.
func listProjectsHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		archived, ok := boolQuery(c, "archived")
		if !ok {
			return
		}

		projects, ok := tenantScoped(c, logger, tenantStore, "project.list.failed",
			func(ctx context.Context, q store.Querier) ([]store.Project, error) {
				if archived {
					return q.ListArchivedProjects(ctx)
				}

				return q.ListProjects(ctx)
			})
		if !ok {
			return
		}

		bodies := make([]projectBody, 0, len(projects))
		for _, project := range projects {
			bodies = append(bodies, newProjectBody(project))
		}

		c.JSON(http.StatusOK, gin.H{"projects": bodies})
	}
}

func getProjectHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := pathUUID(c, "project_id")
		if !ok {
			return
		}

		project, ok := tenantScoped(c, logger, tenantStore, "project.get.failed",
			func(ctx context.Context, q store.Querier) (store.Project, error) {
				project, err := q.GetProject(ctx, projectID)

				return asNotFound(subjectProject, project, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectProject: newProjectBody(project)})
	}
}

func patchProjectHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := pathUUID(c, "project_id")
		if !ok {
			return
		}

		var req patchProjectRequest
		if !bindJSON(c, &req) {
			return
		}

		name, ok := optionalText(c, "name", req.Name, maxNameLength, false)
		if !ok {
			return
		}

		// A description may legitimately be cleared, so an empty string is a
		// value here rather than a validation failure. A name may not: the
		// schema's CHECK forbids it, and a project called "" is unusable.
		description, ok := optionalText(c, "description", req.Description, maxDescriptionLength, true)
		if !ok {
			return
		}

		if name == nil && description == nil {
			c.AbortWithStatusJSON(http.StatusBadRequest,
				errorResponse{Error: "at least one of name or description is required"})

			return
		}

		project, ok := tenantScoped(c, logger, tenantStore, "project.update.failed",
			func(ctx context.Context, q store.Querier) (store.Project, error) {
				project, err := q.UpdateProject(ctx, store.UpdateProjectParams{
					ProjectID:   projectID,
					Name:        name,
					Description: description,
				})

				return asNotFound(subjectProject, project, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectProject: newProjectBody(project)})
	}
}

// archiveProjectHandler is idempotent: archiving an archived project answers
// 200 with the original timestamp rather than 409, so a retry after a dropped
// response is a success.
// archiveProjectHandler hides a project from the default list. It is not a
// delete and it is not a lock.
//
// # The decision #49 asked to make explicit
//
// A board, column or card inside an archived project stays readable and
// writable by id. That was previously true by accident -- nothing cascades on
// archive -- and it is now the intended behaviour, for three reasons:
//
//   - Archiving means "stop showing me this", not "seal it". Somebody following
//     a link to a card from a chat message should get the card, not a wall,
//     and the person who archived the project is usually not the person
//     following the link.
//   - Enforcing otherwise would put a join up to the project on every board,
//     column and card operation -- the hot path -- to guard a state the owner
//     can undo in one request.
//   - Since #49 that undo exists. Archived is no longer terminal, so it does
//     not need the protection a terminal state would.
//
// What archiving does change is discoverability, which was the entire
// complaint: the project leaves the picker, and comes back when unarchived.
func archiveProjectHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := pathUUID(c, "project_id")
		if !ok {
			return
		}

		project, ok := tenantScoped(c, logger, tenantStore, "project.archive.failed",
			func(ctx context.Context, q store.Querier) (store.Project, error) {
				project, err := q.ArchiveProject(ctx, projectID)

				return asNotFound(subjectProject, project, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectProject: newProjectBody(project)})
	}
}

// unarchiveProjectHandler is the way back, and the reason archiving is no
// longer a one-way door.
//
// DELETE on the archive sub-resource rather than an `archived` field on the
// PATCH: it is symmetric with the POST that created it, it is idempotent
// without any extra reasoning, and it keeps the PATCH's "at least one field is
// required" rule about content rather than about state.
//
// # What this deliberately does not do
//
// Nothing cascades, in either direction. A board, column or card inside an
// archived project stays readable and writable by id, which was previously an
// accident and is now a decision -- see the comment on archiveProjectHandler.
func unarchiveProjectHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		projectID, ok := pathUUID(c, "project_id")
		if !ok {
			return
		}

		project, ok := tenantScoped(c, logger, tenantStore, "project.unarchive.failed",
			func(ctx context.Context, q store.Querier) (store.Project, error) {
				project, err := q.UnarchiveProject(ctx, projectID)

				return asNotFound(subjectProject, project, err)
			})
		if !ok {
			return
		}

		c.JSON(http.StatusOK, gin.H{subjectProject: newProjectBody(project)})
	}
}
