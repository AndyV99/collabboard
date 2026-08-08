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

// listProjectsHandler returns the tenant's active projects.
//
// Archived ones are excluded rather than flagged, because ListProjects is the
// query the vault's board picker is built on and an archived project is exactly
// the one nobody wants in that list. There is no way to list archived projects
// yet; that is a gap, not a decision, and it is filed.
func listProjectsHandler(logger *slog.Logger, tenantStore TenantStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		projects, ok := tenantScoped(c, logger, tenantStore, "project.list.failed",
			func(ctx context.Context, q store.Querier) ([]store.Project, error) {
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
