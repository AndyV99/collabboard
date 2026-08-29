package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A failing dependency whose error text carries exactly what must not be
// served: a host, a port, and the name of the driver that produced it. Written
// to look like the real thing rather than like a fixture, because the point of
// the assertion below is that a real driver error would be caught.
type leakyPinger struct{ msg string }

func (l leakyPinger) Ping(context.Context) error { return errors.New(l.msg) }

const (
	leakyPostgres = "failed to connect to `user=collabboard_app database=collabboard`: " +
		"collabboard-staging-postgres.abcdef.us-east-1.rds.amazonaws.com:5432: connection refused"
	leakyRedis = "dial tcp 10.20.36.14:6379: i/o timeout"
)

func healthBody(t *testing.T, disclose bool) (int, []byte, string) {
	t.Helper()

	var logged bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&logged, nil))

	router := NewRouter(logger, BodyLimits{}, nil, HealthDeps{
		Postgres:       leakyPinger{msg: leakyPostgres},
		Redis:          leakyPinger{msg: leakyRedis},
		DiscloseErrors: disclose,
	}, AuthDeps{}, RealtimeDeps{})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	return rec.Code, rec.Body.Bytes(), logged.String()
}

// The deployed policy. This is the assertion that has to fail CI rather than be
// spotted in review, so it greps the whole serialised body for the things that
// must not be in it -- not the `error` field specifically. A future change that
// added a `detail` or `cause` field would slip past a field-level check and
// leak exactly the same information.
func TestDeployedHealthzDisclosesNoTopology(t *testing.T) {
	code, body, _ := healthBody(t, false)

	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 with both dependencies down", code)
	}

	for _, forbidden := range []string{
		"rds.amazonaws.com", // the database hostname
		"10.20.36.14",       // a private address
		"5432", "6379",      // ports
		"connection refused", "i/o timeout", // driver text
		"collabboard_app", // the serving role's name
	} {
		if bytes.Contains(bytes.ToLower(body), []byte(strings.ToLower(forbidden))) {
			t.Errorf("/healthz body discloses %q outside development.\nbody: %s", forbidden, body)
		}
	}
}

// The other half: an operator still has to be able to tell WHICH dependency is
// unhealthy. A redaction that reduced the response to "unavailable" would pass
// the test above and be useless, so this pins the part that must survive.
func TestDeployedHealthzStillNamesTheFailingComponent(t *testing.T) {
	_, body, _ := healthBody(t, false)

	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}

	if got.Status != statusDegrade {
		t.Errorf("overall status = %q, want %q", got.Status, statusDegrade)
	}

	for _, name := range []string{componentPostgres, componentRedis} {
		component, ok := got.Components[name]
		if !ok {
			t.Fatalf("component %q missing from the response entirely", name)
		}

		if component.Status != statusDegrade {
			t.Errorf("component %q status = %q, want %q", name, component.Status, statusDegrade)
		}

		if component.Error != "" {
			t.Errorf("component %q still carries error text %q", name, component.Error)
		}
	}
}

// Suppressed is not lost. If this fails, the redaction has stopped being a
// disclosure policy and started being a hole in the operator's own visibility.
func TestSuppressedDetailStillReachesTheLog(t *testing.T) {
	_, _, logged := healthBody(t, false)

	for _, want := range []string{leakyPostgres, leakyRedis, componentPostgres, componentRedis} {
		if !strings.Contains(logged, want) {
			t.Errorf("log does not carry %q; suppressed detail must still be recorded server-side.\nlog: %s", want, logged)
		}
	}
}

// The local loop is unchanged, which is the reason this is a policy rather than
// a deletion. "connection refused on 5432" is the answer a developer wants, and
// hiding it would move the work to reading logs for no benefit on a machine
// nobody else can reach.
func TestDevelopmentHealthzKeepsTheDetail(t *testing.T) {
	_, body, _ := healthBody(t, true)

	var got healthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}

	if got.Components[componentPostgres].Error != leakyPostgres {
		t.Errorf("development postgres error = %q, want the driver text", got.Components[componentPostgres].Error)
	}

	if got.Components[componentRedis].Error != leakyRedis {
		t.Errorf("development redis error = %q, want the driver text", got.Components[componentRedis].Error)
	}
}

// The zero value must be the safe one. A HealthDeps built without thinking
// about this field -- which is every existing call site in this package's tests
// -- must not disclose. If someone flips the polarity to `SuppressErrors`, this
// is what notices.
func TestHealthDepsZeroValueDoesNotDisclose(t *testing.T) {
	var deps HealthDeps
	if deps.DiscloseErrors {
		t.Fatal("the zero value of HealthDeps must not disclose driver errors: forgetting the field has to fail closed")
	}
}
