package realtime

// The correlation id, joined across two packages (issue #95).
//
// This lives here rather than in internal/api because internal/api cannot
// import internal/realtime — the dependency runs the other way, since the hub
// reads a principal out of the context with api.PrincipalFromContext. So this
// is the only package that can watch one HTTP request produce lines from both.
//
// It is also the pair worth watching. Before #95 the request log was the only
// line carrying a request id, so an operator handed an X-Request-ID by a user
// could find that one line and pivot to nothing.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/logging"
)

// logLines decodes everything written to the buffer, one map per line.
func logLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()

	var lines []map[string]any

	for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if raw == "" {
			continue
		}

		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("log line %q is not JSON: %v", raw, err)
		}

		lines = append(lines, line)
	}

	return lines
}

// lineWithEvent returns the single line carrying an event name.
func lineWithEvent(t *testing.T, lines []map[string]any, event string) map[string]any {
	t.Helper()

	for _, line := range lines {
		if line["event"] == event {
			return line
		}
	}

	t.Fatalf("no line with event %q; got %v", event, lines)

	return nil
}

// TestOneRequestProducesJoinableLinesFromTwoPackages is the acceptance
// criterion of #95.
//
// The request is a plain GET at the WebSocket route with no upgrade headers.
// `websocket.Accept` refuses it, which makes internal/realtime log
// `realtime.upgrade.refused` — while internal/api's requestLogger logs the
// request itself. Two packages, one request, and neither knows about the other.
func TestOneRequestProducesJoinableLinesFromTwoPackages(t *testing.T) {
	t.Parallel()

	logs := &bytes.Buffer{}
	// The production constructor, because the wrapping it does is the feature.
	logger := logging.New(logs, "collabboard-api-test", "debug")

	issuer := testIssuer(t, time.Minute)

	hub, err := NewHub(HubConfig{
		Broker:         NewMemoryBus().Broker(),
		Authorizer:     newScriptedAuthorizer(),
		Logger:         logger,
		AllowedOrigins: []string{"*"},
	})
	if err != nil {
		t.Fatalf("building the hub: %v", err)
	}

	t.Cleanup(func() { _ = hub.Shutdown(t.Context()) })

	router := api.NewRouter(logger, api.BodyLimits{}, nil, api.HealthDeps{},
		api.AuthDeps{Service: stubAuthService{}, Verifier: issuer},
		api.RealtimeDeps{Connect: hub.ConnectHandler(), Publisher: hub.EventPublisher()})

	token, _, err := issuer.Issue(auth.Principal{
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		Role:      auth.RoleOwner,
		SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing a token: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	t.Logf("GET /api/v1/ws (no upgrade headers) -> %d", rec.Code)
	t.Logf("logs:\n%s", logs.String())

	// The id the client was handed. Everything below is joined to *this*, not
	// merely to each other — a pair of lines sharing an id nobody outside the
	// process ever saw would be useless to an operator holding a support ticket.
	handed := rec.Header().Get("X-Request-ID")
	if handed == "" {
		t.Fatal("no X-Request-ID on the response; there is nothing for an operator to search with")
	}

	lines := logLines(t, logs)

	refusal := lineWithEvent(t, lines, "realtime.upgrade.refused")

	if refusal["request_id"] != handed {
		t.Errorf("internal/realtime's line has request_id %v, want %q",
			refusal["request_id"], handed)
	}

	// internal/api's own line, found by its shape rather than by an event name
	// — requestLogger does not set one.
	var requestLine map[string]any

	for _, line := range lines {
		if line["msg"] == "http request" {
			requestLine = line
		}
	}

	if requestLine == nil {
		t.Fatalf("no request log line; got %v", lines)
	}

	if requestLine["request_id"] != handed {
		t.Errorf("internal/api's line has request_id %v, want %q",
			requestLine["request_id"], handed)
	}

	// And exactly once. The field used to be added at the call site; if both
	// mechanisms were live the JSON would carry a duplicate key, which encoding
	// /json emits happily and consumers resolve differently.
	if count := strings.Count(strings.Split(strings.TrimSpace(logs.String()), "\n")[0], `"request_id"`); count > 1 {
		t.Errorf("request_id appears %d times on one line", count)
	}
}

// An inbound X-Request-ID is what the lines carry, so a trace survives the hop
// from the web app.
func TestAnInboundRequestIDReachesEveryLine(t *testing.T) {
	t.Parallel()

	logs := &bytes.Buffer{}
	logger := logging.New(logs, "collabboard-api-test", "debug")

	issuer := testIssuer(t, time.Minute)

	hub, err := NewHub(HubConfig{
		Broker:         NewMemoryBus().Broker(),
		Authorizer:     newScriptedAuthorizer(),
		Logger:         logger,
		AllowedOrigins: []string{"*"},
	})
	if err != nil {
		t.Fatalf("building the hub: %v", err)
	}

	t.Cleanup(func() { _ = hub.Shutdown(t.Context()) })

	router := api.NewRouter(logger, api.BodyLimits{}, nil, api.HealthDeps{},
		api.AuthDeps{Service: stubAuthService{}, Verifier: issuer},
		api.RealtimeDeps{Connect: hub.ConnectHandler(), Publisher: hub.EventPublisher()})

	token, _, err := issuer.Issue(auth.Principal{
		UserID: uuid.New(), TenantID: uuid.New(), Role: auth.RoleOwner, SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing a token: %v", err)
	}

	const fromTheWebApp = "web-abc-123"

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Request-ID", fromTheWebApp)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-ID"); got != fromTheWebApp {
		t.Fatalf("X-Request-ID = %q, want the inbound %q", got, fromTheWebApp)
	}

	refusal := lineWithEvent(t, logLines(t, logs), "realtime.upgrade.refused")

	if refusal["request_id"] != fromTheWebApp {
		t.Errorf("request_id = %v, want the inbound %q", refusal["request_id"], fromTheWebApp)
	}
}

// A line logged outside a request must not gain an empty request_id.
//
// The last acceptance criterion, and the reason WithRequestID refuses to store
// an empty id: a field that is always present and sometimes meaningless is
// worse than one that is absent when there is nothing to say.
func TestBackgroundLinesCarryNoRequestID(t *testing.T) {
	t.Parallel()

	logs := &bytes.Buffer{}
	logger := logging.New(logs, "collabboard-api-test", "debug")

	hub, err := NewHub(HubConfig{
		Broker:     NewMemoryBus().Broker(),
		Authorizer: newScriptedAuthorizer(),
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("building the hub: %v", err)
	}

	// Shutdown logs realtime.hub.draining and realtime.hub.stopped, neither of
	// which belongs to anything a client asked for.
	if err := hub.Shutdown(t.Context()); err != nil {
		t.Fatalf("shutting down: %v", err)
	}

	lines := logLines(t, logs)

	if len(lines) == 0 {
		t.Fatal("the hub logged nothing on shutdown; this test is asserting the absence of a field on no lines")
	}

	for _, line := range lines {
		if _, present := line["request_id"]; present {
			t.Errorf("background line %v carries a request_id", line)
		}
	}
}
