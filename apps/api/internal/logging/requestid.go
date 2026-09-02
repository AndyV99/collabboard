package logging

import (
	"context"
	"log/slog"
)

// RequestIDKey is the field name every correlated line carries.
//
// One constant rather than a literal per call site, because the whole value of
// the field is that an operator can grep for one spelling and get every line
// belonging to one request.
const RequestIDKey = "request_id"

// requestIDContextKey is unexported and of a private type, so nothing outside
// this package can put a value under it -- including by accident, which an
// untyped string key invites across package boundaries.
type requestIDContextKey struct{}

// WithRequestID returns a context carrying the id.
//
// An empty id is not stored, which saves a context value nothing will read.
// [ContextHandler.Handle] guards the same case independently, so removing this
// changes no output -- deliberately: "a line either carries a real id or carries
// no field" is a property worth holding in both directions rather than in the
// one place that happens to be checked first.
func WithRequestID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}

	return context.WithValue(ctx, requestIDContextKey{}, id)
}

// RequestIDFrom returns the id on the context, or "" when there is none.
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	id, _ := ctx.Value(requestIDContextKey{}).(string)

	return id
}

// ContextHandler adds [RequestIDKey] to every record whose context carries one.
//
// # Why a handler rather than fifty edited call sites
//
// The field has to appear on every line logged during a request, and the way to
// make that true tomorrow as well as today is for it to cost nothing at the call
// site. A helper that each line has to remember to call is a helper each line
// can forget; a handler is the difference between "we added the field to the
// lines we could find" and "a line logged during a request has the field".
//
// The cost is that it only works for the `*Context` variants --
// `InfoContext(ctx, ...)`, `LogAttrs(ctx, ...)` -- because those are the ones
// that have a context to read. `logger.Info(...)` is handed
// `context.Background()` by slog and gets nothing, which is the correct answer
// for the startup and background lines that use it: see the tests.
//
// # Why Handle clones the record
//
// slog's handler contract asks a Handler not to modify the Record it is given:
// a Record has value semantics but shares the backing array for attributes past
// its inline capacity, so `AddAttrs` on the parameter can write into memory the
// caller — or a fan-out handler sharing the same record — still owns.
// `record.Clone()` is the documented way to add attributes safely.
//
// Stated rather than tested, deliberately. Removing the Clone breaks no test in
// this repository and I could not construct one that it does break: with a
// single handler the write lands in spare capacity nobody reads again. It is
// kept because the contract says so and because the day a second handler is
// added is not the day to discover this.
type ContextHandler struct {
	slog.Handler
}

// NewContextHandler wraps a handler so that context-carried request ids reach
// the output.
func NewContextHandler(inner slog.Handler) *ContextHandler {
	return &ContextHandler{Handler: inner}
}

// Handle adds the context's request id to the record, when there is one.
func (h *ContextHandler) Handle(ctx context.Context, record slog.Record) error {
	id := RequestIDFrom(ctx)
	if id == "" {
		return h.Handler.Handle(ctx, record)
	}

	cloned := record.Clone()
	cloned.AddAttrs(slog.String(RequestIDKey, id))

	return h.Handler.Handle(ctx, cloned)
}

// WithAttrs and WithGroup re-wrap, which is the whole correctness of this type.
//
// `slog.New(h).With(...)` calls WithAttrs and logs through whatever it returns.
// Without these two methods the embedded handler's versions are promoted, they
// return a bare inner handler, and every logger derived with `.With(...)` --
// which in this service is every logger, since logging.New tags the service name
// that way -- silently stops adding request ids. It is a one-line mistake with
// no symptom except an empty field, so there is a test for it by name.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

// WithGroup re-wraps, for the reason on WithAttrs above.
//
// One caveat, pinned by a test rather than left to be discovered: with a group
// open, [slog.Record.AddAttrs] puts the id *inside* it, so the field is
// `g.request_id` rather than `request_id`. Nothing in this service opens a
// group, and hoisting the id out of one would mean reimplementing the group
// machinery this type exists to wrap.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	return &ContextHandler{Handler: h.Handler.WithGroup(name)}
}
