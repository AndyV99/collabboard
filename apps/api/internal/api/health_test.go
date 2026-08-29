package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type stubPinger struct{ err error }

func (s stubPinger) Ping(context.Context) error { return s.err }

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func doHealthRequest(t *testing.T, deps HealthDeps) (*httptest.ResponseRecorder, healthResponse) {
	t.Helper()

	gin.SetMode(gin.TestMode)

	router := NewRouter(testLogger(), BodyLimits{}, nil, deps, AuthDeps{}, RealtimeDeps{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response body %q: %v", rec.Body.String(), err)
	}

	return rec, body
}

func TestHealthz(t *testing.T) {
	t.Parallel()

	down := errors.New("connection refused")

	tests := map[string]struct {
		deps           HealthDeps
		wantStatus     int
		wantBodyStatus string
		wantComponents map[string]string
	}{
		"all dependencies healthy": {
			deps:           HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
			wantStatus:     http.StatusOK,
			wantBodyStatus: statusOK,
			wantComponents: map[string]string{componentPostgres: statusOK, componentRedis: statusOK},
		},
		"postgres down": {
			deps:           HealthDeps{Postgres: stubPinger{err: down}, Redis: stubPinger{}},
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyStatus: statusDegrade,
			wantComponents: map[string]string{componentPostgres: statusDegrade, componentRedis: statusOK},
		},
		"redis down": {
			deps:           HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{err: down}},
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyStatus: statusDegrade,
			wantComponents: map[string]string{componentPostgres: statusOK, componentRedis: statusDegrade},
		},
		"both down": {
			deps:           HealthDeps{Postgres: stubPinger{err: down}, Redis: stubPinger{err: down}},
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyStatus: statusDegrade,
			wantComponents: map[string]string{componentPostgres: statusDegrade, componentRedis: statusDegrade},
		},
		// A nil dependency must degrade the check, not panic the process.
		"dependency not wired": {
			deps:           HealthDeps{},
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyStatus: statusDegrade,
			wantComponents: map[string]string{componentPostgres: statusDegrade, componentRedis: statusDegrade},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec, body := doHealthRequest(t, tc.deps)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}

			if body.Status != tc.wantBodyStatus {
				t.Errorf("body.status = %q, want %q", body.Status, tc.wantBodyStatus)
			}

			for component, want := range tc.wantComponents {
				got, ok := body.Components[component]
				if !ok {
					t.Fatalf("response is missing component %q", component)
				}

				if got.Status != want {
					t.Errorf("component %q status = %q, want %q", component, got.Status, want)
				}
			}
		})
	}
}

// A dependency whose dial hangs (rather than being refused) must be bounded by
// checkTimeout, and both probes run concurrently, so the whole check costs
// roughly one timeout rather than one per dependency.
func TestHealthzBoundsHangingDependencies(t *testing.T) {
	t.Parallel()

	hang := hangingPinger{}
	start := time.Now()

	rec, body := doHealthRequest(t, HealthDeps{Postgres: hang, Redis: hang})

	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}

	if body.Status != statusDegrade {
		t.Errorf("body.status = %q, want %q", body.Status, statusDegrade)
	}

	if elapsed >= 2*checkTimeout {
		t.Errorf("check took %s; expected roughly one checkTimeout (%s), not one per dependency", elapsed, checkTimeout)
	}
}

type hangingPinger struct{}

func (hangingPinger) Ping(ctx context.Context) error {
	<-ctx.Done()

	return ctx.Err()
}

func TestHealthzSetsRequestID(t *testing.T) {
	t.Parallel()

	rec, _ := doHealthRequest(t, HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}})

	if rec.Header().Get(requestIDHeader) == "" {
		t.Errorf("expected %s response header to be set", requestIDHeader)
	}
}

// A handler that panics must yield a logged 500, not a dead process.
func TestRecoveryMiddleware(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	router := NewRouter(testLogger(), BodyLimits{}, nil,
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}}, AuthDeps{}, RealtimeDeps{})
	router.GET("/boom", func(*gin.Context) { panic("boom") })

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}
