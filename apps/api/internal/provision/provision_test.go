package provision_test

// Validation only. Everything this package does that needs a database is
// exercised by the integration harness, which calls [provision.Roles] to give
// the app role its password on every run — so the connected path is covered by
// every integration test in the repository failing to connect if it breaks.
//
// What is worth testing without a database is the refusals, because they are
// the part that has to hold when the input is wrong rather than when it is
// right.

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/internal/provision"
)

// discardLogger keeps the refusal tests quiet. Nothing below reaches a code
// path that logs, so this only has to be non-nil.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestCredentialValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		credential provision.Credential
		wantErr    bool
		wantIn     string
	}{
		{
			name:       "the role this actually provisions",
			credential: provision.Credential{Role: "collabboard_app", Password: "s3cret"},
		},
		{
			name:       "empty role",
			credential: provision.Credential{Role: "", Password: "s3cret"},
			wantErr:    true,
			wantIn:     "role is empty",
		},
		{
			// The refusal that matters most. ALTER ROLE ... PASSWORD '' does
			// not fail, it sets the password to null — turning a role that
			// needed a secret into one that needs none.
			name:       "empty password",
			credential: provision.Credential{Role: "collabboard_app", Password: ""},
			wantErr:    true,
			wantIn:     "would clear the password rather than set one",
		},
		{
			name:       "role with a quote in it",
			credential: provision.Credential{Role: `app"; DROP TABLE users; --`, Password: "s3cret"},
			wantErr:    true,
			wantIn:     "not a plain lower-case identifier",
		},
		{
			name:       "role with a space",
			credential: provision.Credential{Role: "collabboard app", Password: "s3cret"},
			wantErr:    true,
			wantIn:     "not a plain lower-case identifier",
		},
		{
			// Mixed case is rejected rather than folded. PostgreSQL would
			// lower-case an unquoted CollabBoard_App and quote-preserve a
			// quoted one, so accepting it here means the role altered depends
			// on how the DDL was built — which is exactly the ambiguity this
			// check exists to remove.
			name:       "mixed case role",
			credential: provision.Credential{Role: "CollabBoard_App", Password: "s3cret"},
			wantErr:    true,
			wantIn:     "not a plain lower-case identifier",
		},
		{
			name:       "role starting with a digit",
			credential: provision.Credential{Role: "1app", Password: "s3cret"},
			wantErr:    true,
			wantIn:     "not a plain lower-case identifier",
		},
		{
			name:       "digits after the first character are fine",
			credential: provision.Credential{Role: "collabboard_app2", Password: "s3cret"},
		},
		{
			// PostgreSQL truncates at NAMEDATALEN-1 silently, so a name one
			// byte too long would alter a different role than the one
			// configured.
			name:       "role longer than NAMEDATALEN",
			credential: provision.Credential{Role: strings.Repeat("a", 64), Password: "s3cret"},
			wantErr:    true,
			wantIn:     "PostgreSQL truncates",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.credential.Validate()

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}

				return
			}

			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}

			if !errors.Is(err, provision.ErrInvalidCredential) {
				t.Errorf("error does not wrap ErrInvalidCredential: %v", err)
			}

			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

// A bad credential must be rejected before anything opens a connection, so a
// misconfiguration names the offending value instead of timing out against a
// database it should never have dialled.
func TestRolesRejectsBadCredentialsWithoutConnecting(t *testing.T) {
	t.Parallel()

	// A DSN pointing at a port nothing listens on. If Roles validated after
	// connecting, this would surface as a dial error rather than the
	// validation error asserted below.
	const unreachable = "postgres://nobody:nothing@127.0.0.1:1/collabboard?sslmode=disable&connect_timeout=1"

	err := provision.Roles(t.Context(), discardLogger(), unreachable,
		provision.Credential{Role: "collabboard_app", Password: ""})
	if !errors.Is(err, provision.ErrInvalidCredential) {
		t.Fatalf("Roles() = %v, want ErrInvalidCredential", err)
	}
}

func TestRolesRejectsAnEmptyCredentialList(t *testing.T) {
	t.Parallel()

	err := provision.Roles(t.Context(), discardLogger(), "postgres://ignored/db")
	if !errors.Is(err, provision.ErrInvalidCredential) {
		t.Fatalf("Roles() = %v, want ErrInvalidCredential", err)
	}
}

// Describe exists so a caller building a log line cannot reach for the struct
// and include the password by accident. That is only true if it never returns
// one.
func TestDescribeNamesRolesAndNotSecrets(t *testing.T) {
	t.Parallel()

	got := provision.Describe([]provision.Credential{
		{Role: "collabboard_app", Password: "hunter2"},
		{Role: "collabboard_other", Password: "hunter3"},
	})

	if want := "collabboard_app, collabboard_other"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}

	for _, secret := range []string{"hunter2", "hunter3"} {
		if strings.Contains(got, secret) {
			t.Errorf("Describe() leaked %q", secret)
		}
	}
}
