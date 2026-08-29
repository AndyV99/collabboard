package auth_test

// The bound on organizations.name (issue #67).
//
// This was the one user-supplied field on the API with no length check at all.
// Every other name a caller can set is capped at 200 runes by internal/api's
// maxNameLength; this one was trimmed, defaulted when blank, and inserted. #50's
// 16 KiB body limit made the practical ceiling "whatever fits", which is a
// containment rather than a validation: an 8,000-character workspace name was
// accepted, stored, turned into a slug, and rendered into every member's UI.
//
// It is also the only such field reachable **without a credential**, which is
// why it is worth a test of its own rather than another row in the registration
// table.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// maxWorkspaceNameRunes mirrors maxOrganizationNameLength, which is unexported.
//
// Repeated rather than exported for a test: the number is part of the API's
// contract with its callers, and a test that read it out of the package could
// not fail when the package changed it. This is the same reason the web's
// MAX_ORGANIZATION_NAME_CODE_POINTS is written out rather than fetched.
const maxWorkspaceNameRunes = 200

func TestRegistrationBoundsTheWorkspaceName(t *testing.T) {
	t.Parallel()

	// Emoji, not "a". The check counts runes because every other name check on
	// this API counts runes, and a byte- or UTF-16-based one would refuse a
	// name that is exactly at the limit. 200 U+1F642 is 200 runes and 800
	// bytes; a `len()` check would reject it at a quarter of the real limit.
	const atTheLimit = maxWorkspaceNameRunes

	emoji := strings.Repeat("\U0001F642", atTheLimit)

	cases := []struct {
		name string
		in   auth.RegisterInput
		want error
	}{
		{
			name: "exactly at the limit is accepted",
			in: auth.RegisterInput{
				Email: "at@example.com", Password: testPassword, DisplayName: "Someone",
				OrganizationName: strings.Repeat("a", atTheLimit),
			},
			want: nil,
		},
		{
			name: "one rune over is refused",
			in: auth.RegisterInput{
				Email: "over@example.com", Password: testPassword, DisplayName: "Someone",
				OrganizationName: strings.Repeat("a", atTheLimit+1),
			},
			want: auth.ErrInvalidInput,
		},
		{
			name: "the count is runes, so 200 emoji fit",
			in: auth.RegisterInput{
				Email: "emoji@example.com", Password: testPassword, DisplayName: "Someone",
				OrganizationName: emoji,
			},
			want: nil,
		},
		{
			name: "201 emoji do not",
			in: auth.RegisterInput{
				Email: "emojiover@example.com", Password: testPassword, DisplayName: "Someone",
				OrganizationName: emoji + "\U0001F642",
			},
			want: auth.ErrInvalidInput,
		},
		{
			// The old ceiling, from #50's body limit. Well inside 16 KiB and
			// well outside anything a workspace is called.
			name: "the size that used to be accepted is not",
			in: auth.RegisterInput{
				Email: "essay@example.com", Password: testPassword, DisplayName: "Someone",
				OrganizationName: strings.Repeat("a", 8000),
			},
			want: auth.ErrInvalidInput,
		},
		{
			// Trimmed before it is counted, the same way workspaceName trims
			// before it decides whether a name was given at all.
			name: "surrounding whitespace does not count against the limit",
			in: auth.RegisterInput{
				Email: "padded@example.com", Password: testPassword, DisplayName: "Someone",
				OrganizationName: "   " + strings.Repeat("a", atTheLimit) + "   ",
			},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, generousLimits())

			_, err := h.service.Register(context.Background(), tc.in)

			t.Logf("%s -> %v", tc.name, err)

			if !errors.Is(err, tc.want) {
				t.Fatalf("Register = %v, want %v", err, tc.want)
			}
		})
	}
}

// The refusal must happen before the account exists.
//
// Registration is two transactions — a pre-tenant one creating the user, then a
// tenant-scoped one creating the organization — and a failure between them
// strands an account that can authenticate and belongs nowhere (#34, ADR 0009).
// A bound enforced only at the write would turn a fixable typo into a way of
// manufacturing that state on demand, one request at a time.
func TestAnOverLongWorkspaceNameCreatesNoAccountAtAll(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	const email = "would-be-stranded@example.com"

	_, err := h.service.Register(context.Background(), auth.RegisterInput{
		Email:            email,
		Password:         testPassword,
		DisplayName:      "Someone",
		OrganizationName: strings.Repeat("a", maxWorkspaceNameRunes+1),
	})
	if !errors.Is(err, auth.ErrInvalidInput) {
		t.Fatalf("Register = %v, want %v", err, auth.ErrInvalidInput)
	}

	// The account must not exist. Login is the question a user would ask, and
	// ErrNoOrganization here would be exactly the stranded state.
	_, lerr := h.service.Login(context.Background(), auth.LoginInput{
		Email: email, Password: testPassword, ClientIP: "203.0.113.10",
	})

	if errors.Is(lerr, auth.ErrNoOrganization) {
		t.Fatal("the refused registration left an account with no organization; " +
			"the bound is being applied at the write instead of before it")
	}

	if !errors.Is(lerr, auth.ErrInvalidCredentials) {
		t.Errorf("Login after a refused registration = %v, want %v",
			lerr, auth.ErrInvalidCredentials)
	}

	// And the operator signal for a half-registration must not have fired.
	if strings.Contains(h.logs.String(), "auth.register.partial") {
		t.Error("a refused registration logged auth.register.partial; nothing was half-created")
	}
}

// The generated default is subject to the same cap.
//
// It cannot exceed it today: a display name is capped at 128 runes and the
// default is that plus "'s workspace", which is 140. That is a fact about
// maxDisplayNameLength rather than about this rule, and the two numbers have no
// reason to know about each other — so the default is validated rather than
// exempted, and this asserts the case that exists now.
func TestTheGeneratedWorkspaceNameIsWithinTheBound(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	longestDisplayName := strings.Repeat("n", 128)

	result, err := h.service.Register(context.Background(), auth.RegisterInput{
		Email:       "default@example.com",
		Password:    testPassword,
		DisplayName: longestDisplayName,
		// No OrganizationName: the default is what is under test.
	})
	if err != nil {
		t.Fatalf("Register with the longest permitted display name: %v", err)
	}

	if got := len([]rune(result.OrganizationName)); got > maxWorkspaceNameRunes {
		t.Fatalf("the generated default is %d runes, over the %d-rune bound",
			got, maxWorkspaceNameRunes)
	}

	if !strings.HasSuffix(result.OrganizationName, "'s workspace") {
		t.Errorf("OrganizationName = %q, want the generated default", result.OrganizationName)
	}
}

// The recovery path is bounded too.
//
// Register checks early to avoid the stranded state; this one is covered by the
// guard inside provisionOrganization, which every writer of organizations.name
// goes through. Asserting it separately is what makes that guard load-bearing
// rather than decorative — without it, the only enforcement would be in a
// function this path does not call.
func TestCreateFirstOrganizationBoundsTheWorkspaceName(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	const email = "stranded-and-shouting@example.com"

	h.strand(t, email, "Stranded Person")

	_, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
		Email:            email,
		Password:         testPassword,
		OrganizationName: strings.Repeat("a", maxWorkspaceNameRunes+1),
		ClientIP:         "198.51.100.7",
	})
	if !errors.Is(err, auth.ErrInvalidInput) {
		t.Fatalf("CreateFirstOrganization = %v, want %v", err, auth.ErrInvalidInput)
	}

	// Still recoverable afterwards: a refused name must not consume the one
	// chance this account has to leave the stranded state.
	created, err := h.service.CreateFirstOrganization(context.Background(), auth.CreateOrganizationInput{
		Email:            email,
		Password:         testPassword,
		OrganizationName: "Second Time Lucky",
		ClientIP:         "198.51.100.7",
	})
	if err != nil {
		t.Fatalf("CreateFirstOrganization after a refused name: %v", err)
	}

	if created.OrganizationName != "Second Time Lucky" {
		t.Errorf("OrganizationName = %q, want %q", created.OrganizationName, "Second Time Lucky")
	}
}
