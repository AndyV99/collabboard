package config

// The request body limits.
//
// They are a security control rather than a tuning knob, and their failure mode
// is silence: a limit of zero, or one that is unreachable because a looser one
// is applied first, looks configured and enforces nothing. Load refuses all of
// those shapes, and these tests are what keep that true.

import (
	"strings"
	"testing"
)

func TestLoadRejectsABodyLimitThatWouldEnforceNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		env   map[string]string
		wants string
	}{
		{
			name:  "a global limit of zero",
			env:   map[string]string{"HTTP_MAX_REQUEST_BYTES": "0"},
			wants: "HTTP_MAX_REQUEST_BYTES",
		},
		{
			name:  "a negative global limit",
			env:   map[string]string{"HTTP_MAX_REQUEST_BYTES": "-1"},
			wants: "HTTP_MAX_REQUEST_BYTES",
		},
		{
			name:  "an unauthenticated limit of zero",
			env:   map[string]string{"HTTP_MAX_UNAUTHENTICATED_REQUEST_BYTES": "0"},
			wants: "HTTP_MAX_UNAUTHENTICATED_REQUEST_BYTES",
		},
		{
			name: "an unauthenticated limit the global one would refuse first",
			env: map[string]string{
				"HTTP_MAX_REQUEST_BYTES":                 "1024",
				"HTTP_MAX_UNAUTHENTICATED_REQUEST_BYTES": "4096",
			},
			wants: "would never apply",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			_, err := Load()
			if err == nil {
				t.Fatal("Load() accepted a body limit that enforces nothing")
			}

			t.Logf("%s -> %v", tc.name, err)

			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error %q does not mention %q", err, tc.wants)
			}
		})
	}
}

func TestTheBodyLimitDefaultsClearTheLargestLegitimateRequest(t *testing.T) {
	for _, key := range []string{"HTTP_MAX_REQUEST_BYTES", "HTTP_MAX_UNAUTHENTICATED_REQUEST_BYTES"} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if got, want := cfg.HTTP.MaxRequestBytes, DefaultMaxRequestBytes; got != want {
		t.Errorf("HTTP.MaxRequestBytes = %d, want %d", got, want)
	}

	if got, want := cfg.HTTP.MaxUnauthenticatedRequestBytes, DefaultMaxUnauthenticatedRequestBytes; got != want {
		t.Errorf("HTTP.MaxUnauthenticatedRequestBytes = %d, want %d", got, want)
	}

	// The global default has to clear the largest body internal/api's own field
	// limits permit — a card with a 200 rune title and a 10 000 rune
	// description, every rune escaped as a twelve byte surrogate pair — or the
	// limit would be refusing requests the handlers would have accepted. That
	// arithmetic is the reason the number is 256 KiB and not a round guess, so
	// it is worth asserting rather than describing.
	const largestLegitimateBody = (200 + 10000) * 12

	if cfg.HTTP.MaxRequestBytes < largestLegitimateBody {
		t.Errorf("HTTP.MaxRequestBytes = %d, under the %d bytes a card write can legitimately weigh",
			cfg.HTTP.MaxRequestBytes, largestLegitimateBody)
	}

	// And the unauthenticated one has to clear the largest registration: a 254
	// byte address, a 128 byte password and a 128 rune display name, escaped the
	// same way, plus room for an organization name.
	const largestRegistration = (254+128)*6 + 128*12

	if cfg.HTTP.MaxUnauthenticatedRequestBytes < largestRegistration {
		t.Errorf("HTTP.MaxUnauthenticatedRequestBytes = %d, under the %d bytes a registration can weigh",
			cfg.HTTP.MaxUnauthenticatedRequestBytes, largestRegistration)
	}
}
