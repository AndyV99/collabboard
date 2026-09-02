package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line %q is not JSON: %v", buf.String(), err)
	}

	return line
}

func TestContextHandlerAddsTheRequestID(t *testing.T) {
	var buf bytes.Buffer

	logger := New(&buf, "svc", "info")

	logger.InfoContext(WithRequestID(context.Background(), "req-1"), "hello")

	if got := decode(t, &buf)[RequestIDKey]; got != "req-1" {
		t.Errorf("%s = %v, want %q", RequestIDKey, got, "req-1")
	}
}

// The one-line mistake with no symptom.
//
// `slog.New(h).With(...)` calls WithAttrs and logs through what it returns. If
// ContextHandler did not re-wrap there, the embedded handler's method would be
// promoted, it would return a bare inner handler, and every derived logger
// would silently stop adding request ids.
//
// This is not hypothetical for this service: [New] itself ends in
// `.With(slog.String("service", service))`, so *every* logger in the process is
// a derived one. A ContextHandler that only worked before `.With` would work
// nowhere.
func TestTheRequestIDSurvivesWithAttrsAndWithGroup(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req-2")

	t.Run("With", func(t *testing.T) {
		var buf bytes.Buffer

		New(&buf, "svc", "info").With(slog.String("extra", "x")).InfoContext(ctx, "hello")

		line := decode(t, &buf)

		if line[RequestIDKey] != "req-2" {
			t.Errorf("%s = %v, want %q", RequestIDKey, line[RequestIDKey], "req-2")
		}

		if line["extra"] != "x" {
			t.Errorf("the derived attr was lost: %v", line)
		}
	})

	t.Run("WithGroup", func(t *testing.T) {
		var buf bytes.Buffer

		New(&buf, "svc", "info").WithGroup("g").InfoContext(ctx, "hello")

		/*
		 * The id is still added -- and it lands *inside* the group, because
		 * that is what slog.Record.AddAttrs does once a group is open. Pinned
		 * as the known behaviour rather than the desired one.
		 *
		 * Nothing in this service opens a group (checked: zero call sites
		 * outside this package), so it costs nothing today. It is asserted
		 * anyway because the first line that does open one would otherwise
		 * discover this by an operator's grep for `"request_id"` returning
		 * fewer lines than there are requests -- and getting the id to the top
		 * level regardless of groups is not something a wrapping handler can do
		 * without reimplementing the group machinery it wraps.
		 */
		group, ok := decode(t, &buf)["g"].(map[string]any)
		if !ok {
			t.Fatalf("no group in the output: %s", buf.String())
		}

		if group[RequestIDKey] != "req-2" {
			t.Errorf("g.%s = %v, want %q", RequestIDKey, group[RequestIDKey], "req-2")
		}
	})
}

func TestNoRequestIDMeansNoField(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"a bare context", context.Background()},
		{"an explicitly empty id", WithRequestID(context.Background(), "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer

			New(&buf, "svc", "info").InfoContext(tc.ctx, "hello")

			if _, present := decode(t, &buf)[RequestIDKey]; present {
				t.Errorf("a line with no request id carries %q: %s", RequestIDKey, buf.String())
			}
		})
	}
}

// The non-context variants are the background lines, and they are unaffected.
func TestTheNonContextVariantsGetNothing(t *testing.T) {
	var buf bytes.Buffer

	New(&buf, "svc", "info").Info("startup")

	if _, present := decode(t, &buf)[RequestIDKey]; present {
		t.Errorf("logger.Info gained a %q: %s", RequestIDKey, buf.String())
	}
}
