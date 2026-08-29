package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// assigneeAndDueDate is the shared parsing for the two card fields that reach
// nullable columns, used by both create and patch so the two cannot disagree
// about what a valid request looks like.

// parseAssignee turns a submitted assignee id into a uuid, or aborts with 400.
//
// It does NOT check membership -- that needs a tenant-scoped query and belongs
// inside the transaction, next to the write it authorises. See
// assigneeIsMember.
func parseAssignee(c *gin.Context, raw *string) (*uuid.UUID, bool) {
	if raw == nil {
		return nil, true
	}

	id, err := uuid.Parse(*raw)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			errorResponse{Error: "assignee_id must be a uuid"})

		return nil, false
	}

	return &id, true
}

// parseDueAt turns a submitted due date into a time, or aborts with 400.
//
// RFC 3339 with an offset required, which is what time.RFC3339 parses and what
// the column stores -- `timestamptz` has no local time to fall back on, so an
// input without an offset would be interpreted against the server's zone and
// mean something different depending on where it ran.
func parseDueAt(c *gin.Context, raw *string) (*time.Time, bool) {
	if raw == nil {
		return nil, true
	}

	at, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest,
			errorResponse{Error: "due_at must be an RFC 3339 timestamp with an offset, for example 2026-01-31T17:00:00Z"})

		return nil, false
	}

	return &at, true
}

// assigneeNotAMember refuses an assignee this organization cannot assign to.
//
// 400 rather than 404, because the card was found and it is the submitted
// *field* that is wrong -- the same answer shape as "assignee_id must be a
// uuid", which is the other way this field can be unusable.
//
// One constant sentence, and one for three different situations. It must not
// say which -- see assigneeIsMember for why -- and, like `conflict`, it is used
// both as the response body and as a log `detail`, so it must never interpolate
// anything read out of the database.
func assigneeNotAMember() *apiError {
	return &apiError{
		status:   http.StatusBadRequest,
		message:  "assignee_id must name a member of this organization",
		logEvent: "card.assignee.refused",
	}
}

// assigneeIsMember refuses an assignee this tenant cannot assign to.
//
// # Why this is not a membership oracle
//
// GetMembership runs inside the tenant transaction, so row-level security
// scopes it to the caller's organization. Three different situations therefore
// produce the identical answer -- no row:
//
//   - the user id names nobody at all
//   - the user exists and belongs to a different organization
//   - the user was a member here and their membership was revoked
//
// The caller cannot tell them apart, which is the point: a distinguishable
// answer would let anyone with a card to edit probe for the existence of user
// ids across the tenant boundary, one guess at a time. The same reasoning as
// crud.go's `notFound`, one layer down.
//
// # Why check at all when the foreign key already refuses it
//
// (tenant_id, assignee_id) references memberships, so the database would reject
// this anyway. It would reject it as a constraint violation, which nothing maps
// -- so it would surface as 500, which is the shape of bug #76 was about. The
// constraint is the backstop; this is the answer.
func assigneeIsMember(ctx context.Context, q store.Querier, assignee uuid.UUID) error {
	membership, err := q.GetMembership(ctx, assignee)
	if err != nil {
		if errors.Is(err, store.ErrNoRows) {
			return assigneeNotAMember()
		}

		return err
	}

	if membership.UserID != assignee {
		return assigneeNotAMember()
	}

	return nil
}
