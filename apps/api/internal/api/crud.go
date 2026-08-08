package api

// The plumbing shared by the project/board/column/card handlers.
//
// # The one thing this file exists to guarantee
//
// [tenantScoped] is the only expression in this package that hands a tenant id
// to the data layer. Every CRUD handler goes through it, it reads the tenant
// from [principalFrom] — that is, from the verified token claim — and it takes
// no tenant argument, so there is nothing for a handler to pass and nothing for
// a handler to get wrong.
//
// That is deliberate. ADR 0001 is explicit that row-level security is isolation
// and not authorization: Postgres will serve whatever tenant it is told about.
// With twenty-odd handlers, "the tenant comes from the claim" stops being a fact
// anyone can check by reading them all, and becomes a fact about one function.
// `grep -n 'principal.TenantID' internal/api` is the audit, and it should return
// this file and auth.go's members handler.
//
// # Why an id in the path is not an authorization decision
//
// Every handler below takes an object id from the URL, and none of them checks
// who owns it. They do not have to: the id is used as a predicate inside a
// transaction already scoped to the caller's tenant, so an id belonging to
// another organization matches no row and the query reports "no rows" — the same
// answer as an id that never existed. The handler turns that into 404. There is
// no branch where the object is found and then rejected, which is the branch
// that gets written wrong.
//
// crud_bola_test.go attacks exactly this, on every endpoint.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// The four things this surface addresses.
//
// One constant each rather than a literal per use, because the same word is
// both the JSON envelope key ({"card": {...}}) and the subject of the 404 that
// says the id named nothing. Those two agreeing is what makes "card not found"
// legible next to a response whose payload would have been "card".
const (
	subjectProject = "project"
	subjectBoard   = "board"
	subjectColumn  = "column"
	subjectCard    = "card"
)

// Field limits. The schema only says "not blank", so without these a name is
// bounded by the request body size, which is bounded by nothing. They are
// counted in runes rather than bytes so the limit means the same thing to
// someone writing Japanese as to someone writing English.
const (
	maxNameLength        = 200
	maxDescriptionLength = 10000
)

// apiError is an error a handler's callback can return to choose its own status.
//
// It exists because the interesting failures happen *inside* the tenant
// transaction — "that column is on another board" is only knowable once the
// column has been read — and by then the callback is three frames below the
// *gin.Context. Returning the status as a value keeps the response writing in
// one place instead of passing the context down.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string {
	return e.message
}

// notFound is the answer for every id that names nothing the caller can see.
//
// "Nothing the caller can see" deliberately includes another tenant's object.
// Answering 403 there would confirm the id exists, which is an existence oracle
// across the tenant boundary — the same reasoning that makes an unjoined
// organization and a fictional one both 403 in switchOrganizationHandler.
func notFound(subject string) *apiError {
	return &apiError{status: http.StatusNotFound, message: subject + " not found"}
}

// conflict is for a request that is well-formed and authorized but disagrees
// with the current state of the board — a stale drag-and-drop, usually.
func conflict(message string) *apiError {
	return &apiError{status: http.StatusConflict, message: message}
}

// asNotFound turns a query's "no rows" into a 404 that names what was missing.
//
// Under row-level security "no rows" is also the answer for a row belonging to
// another tenant, which is exactly why this mapping is centralised: every
// caller should produce the same 404 for both, and none of them should be
// deciding that for itself.
func asNotFound[T any](subject string, value T, err error) (T, error) {
	if errors.Is(err, store.ErrNoRows) {
		var zero T

		return zero, notFound(subject)
	}

	return value, err
}

// tenantScoped runs fn inside a transaction scoped to the authenticated
// principal's tenant, and writes the response for every failure path.
//
// The second return value is false when a response has already been written, so
// a caller's whole error handling is `if !ok { return }`.
//
// It is a function rather than a method because Go methods cannot take type
// parameters — the same reason [store.InTenant] is one.
func tenantScoped[T any](
	c *gin.Context,
	logger *slog.Logger,
	tenantStore TenantStore,
	event string,
	fn func(ctx context.Context, q store.Querier) (T, error),
) (T, bool) {
	var zero T

	principal, ok := principalFrom(c)
	if !ok {
		rejectUnauthenticated(c, logger, "no_principal", nil)

		return zero, false
	}

	var out T

	// The tenant, from the claim and from nowhere else. This is the line the
	// package comment is about.
	err := tenantStore.WithTenant(c.Request.Context(), principal.TenantID,
		func(ctx context.Context, q store.Querier) error {
			var ferr error

			out, ferr = fn(ctx, q)

			return ferr
		})
	if err != nil {
		writeStoreError(c, logger, event, principal.TenantID, err)

		return zero, false
	}

	return out, true
}

// tenantScopedPublish is [tenantScoped] plus the broadcast, and the order of
// those two is the entire reason it exists.
//
// describe runs only after WithTenant has returned nil — that is, after the
// transaction has committed. fn cannot publish instead, because fn is handed a
// [store.Querier] and nothing else: the publisher is not in scope inside the
// transaction, on this handler or on any future one. "A rolled-back write
// broadcasts nothing" is therefore something the compiler enforces rather than
// something a reviewer has to check. See events.go, and
// TestARolledBackWriteBroadcastsNothing.
//
// describe may return the zero [BoardEvent] for a write with nothing worth
// announcing; publishBoardEvent then does nothing.
func tenantScopedPublish[T any](
	c *gin.Context,
	logger *slog.Logger,
	tenantStore TenantStore,
	publisher EventPublisher,
	event string,
	fn func(ctx context.Context, q store.Querier) (T, error),
	describe func(T) BoardEvent,
) (T, bool) {
	out, ok := tenantScoped(c, logger, tenantStore, event, fn)
	if !ok {
		return out, false
	}

	publishBoardEvent(c, logger, publisher, describe(out))

	return out, true
}

// writeStoreError maps a failed transaction onto a status.
//
// The default is 500 with a generic body and the real error in the log, so a
// constraint violation or a driver error can never become a response. A bare
// [store.ErrNoRows] escaping a callback that did not wrap it is still a 404 —
// under row-level security "no rows" is what "not yours" looks like, and 500
// would be both wrong and a hint that the row is real.
func writeStoreError(c *gin.Context, logger *slog.Logger, event string, tenantID uuid.UUID, err error) {
	var apiErr *apiError

	switch {
	case errors.As(err, &apiErr):
		c.AbortWithStatusJSON(apiErr.status, errorResponse{Error: apiErr.message})

	case errors.Is(err, store.ErrNoRows):
		c.AbortWithStatusJSON(http.StatusNotFound, errorResponse{Error: "not found"})

	default:
		logger.ErrorContext(c.Request.Context(), "request failed",
			slog.String("event", event),
			slog.String("path", c.FullPath()),
			slog.String("tenant_id", tenantID.String()),
			slog.Any("error", err),
		)

		c.AbortWithStatusJSON(http.StatusInternalServerError, errorResponse{Error: messageInternalError})
	}
}

// pathUUID reads a uuid route parameter.
//
// A malformed id is 400 rather than 404: it is a claim about the request's
// syntax, not about what exists, and answering 404 would make a typo
// indistinguishable from a deleted board.
func pathUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: name + " must be a uuid"})

		return uuid.Nil, false
	}

	return id, true
}

// optionalUUID reads a uuid that a request body may omit or send as null.
//
// The nil return is meaningful: for a move it is "put this first", a position no
// sibling's id can name.
func optionalUUID(c *gin.Context, field string, value *string) (*uuid.UUID, bool) {
	if value == nil || *value == "" {
		return nil, true
	}

	id, err := uuid.Parse(*value)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: field + " must be a uuid"})

		return nil, false
	}

	return &id, true
}

// requiredText validates a field the schema requires to be non-blank.
//
// Trimmed before the check *and* before storage, so " " is rejected rather than
// stored as a name that renders as nothing. The database has the same CHECK; the
// point of repeating it is the status code, since a constraint violation would
// surface as a 500.
func requiredText(c *gin.Context, field, value string, limit int) (string, bool) {
	trimmed := strings.TrimSpace(value)

	if trimmed == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: field + " is required"})

		return "", false
	}

	if utf8.RuneCountInString(trimmed) > limit {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: field + " is too long"})

		return "", false
	}

	return trimmed, true
}

// boundedText validates a field that may be empty but not unbounded.
func boundedText(c *gin.Context, field, value string, limit int) (string, bool) {
	trimmed := strings.TrimSpace(value)

	if utf8.RuneCountInString(trimmed) > limit {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: field + " is too long"})

		return "", false
	}

	return trimmed, true
}

// optionalText validates a field a PATCH may omit. A nil pointer stays nil,
// which the query reads as "leave this column alone".
func optionalText(c *gin.Context, field string, value *string, limit int, allowEmpty bool) (*string, bool) {
	if value == nil {
		return nil, true
	}

	trimmed := strings.TrimSpace(*value)

	if trimmed == "" && !allowEmpty {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: field + " cannot be empty"})

		return nil, false
	}

	if utf8.RuneCountInString(trimmed) > limit {
		c.AbortWithStatusJSON(http.StatusBadRequest, errorResponse{Error: field + " is too long"})

		return nil, false
	}

	return &trimmed, true
}

// timestamp renders a database timestamp the one way this API renders them.
func timestamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// optionalTimestamp renders a nullable timestamp, keeping JSON null for "never".
func optionalTimestamp(t *time.Time) *string {
	if t == nil {
		return nil
	}

	rendered := timestamp(*t)

	return &rendered
}
