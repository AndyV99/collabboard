package auth_test

// Service.Me at the service layer (issue #75).
//
// The HTTP-shaped claims — that no header, query parameter or path segment can
// steer the endpoint — live in internal/api/auth_bola_test.go, and the
// end-to-end proof against a real database and real policies lives in
// internal/api/me_integration_test.go. These are the claims about the
// *decision*: which row is read, whose it is, and what happens when the
// membership behind the token is gone.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// registerNamed is register with a display name of its own, since telling two
// accounts apart is the whole point of this file.
func registerNamed(t *testing.T, h *harness, email, displayName string) auth.RegisterResult {
	t.Helper()

	result, err := h.service.Register(context.Background(), auth.RegisterInput{
		Email:       email,
		Password:    testPassword,
		DisplayName: displayName,
	})
	if err != nil {
		t.Fatalf("Register(%s): %v", email, err)
	}

	return result
}

// TestMeReportsTheCallersOwnIdentity is the control, and the acceptance
// criterion: two accounts in the same organization each see themselves.
func TestMeReportsTheCallersOwnIdentity(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := registerNamed(t, h, "alice@example.com", "Alice Owner")
	bob := registerNamed(t, h, "bob@example.com", "Bob Member")

	// One organization, two members — alice's. Without the addition each of
	// them would be alone in their own tenant, and "you get your own row"
	// would also hold for a query that returned the only row it could see.
	if _, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: principalFor(alice, auth.RoleOwner),
		Email:     bob.Email,
	}); err != nil {
		t.Fatalf("adding bob to alice's organization: %v", err)
	}

	for _, subject := range []struct {
		name        string
		principal   auth.Principal
		wantID      uuid.UUID
		wantEmail   string
		wantDisplay string
		otherEmail  string
	}{
		{
			name:        "alice",
			principal:   principalFor(alice, auth.RoleOwner),
			wantID:      alice.UserID,
			wantEmail:   "alice@example.com",
			wantDisplay: "Alice Owner",
			otherEmail:  "bob@example.com",
		},
		{
			// Bob's principal is alice's *tenant* with bob's user id: that is
			// the token he holds after switching into her organization, and it
			// is the case where a query that ignored the user id would hand him
			// somebody else's identity.
			name: "bob, in alice's organization",
			principal: auth.Principal{
				UserID:    bob.UserID,
				TenantID:  alice.OrganizationID,
				Role:      auth.RoleMember,
				SessionID: uuid.New(),
			},
			wantID:      bob.UserID,
			wantEmail:   "bob@example.com",
			wantDisplay: "Bob Member",
			otherEmail:  "alice@example.com",
		},
	} {
		t.Run(subject.name, func(t *testing.T) {
			me, err := h.service.Me(context.Background(), subject.principal)
			if err != nil {
				t.Fatalf("Me(%s): %v", subject.name, err)
			}

			t.Logf("%s -> %s / %q, %d organization(s)",
				subject.name, me.Profile.Email, me.Profile.DisplayName, len(me.Organizations))

			if me.Profile.UserID != subject.wantID {
				t.Errorf("user id = %s, want %s", me.Profile.UserID, subject.wantID)
			}

			if me.Profile.Email != subject.wantEmail {
				t.Errorf("email = %q, want %q", me.Profile.Email, subject.wantEmail)
			}

			if me.Profile.DisplayName != subject.wantDisplay {
				t.Errorf("display name = %q, want %q", me.Profile.DisplayName, subject.wantDisplay)
			}

			if me.Profile.Email == subject.otherEmail {
				t.Errorf("Me returned the other member's address")
			}

			if len(me.Organizations) == 0 {
				t.Error("Me returned no organizations; the existing half of /me regressed")
			}
		})
	}
}

// TestMeReadsTheIdentityInTheCallersOwnTenant pins where the data comes from.
//
// The identity half is an ordinary tenant-scoped read, and the tenant it is
// read in is the principal's. If it ever became a pre-tenant lookup the door
// would be a fifth reason wide and this assertion would be the thing that
// noticed.
func TestMeReadsTheIdentityInTheCallersOwnTenant(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := registerNamed(t, h, "alice@example.com", "Alice Owner")

	before := len(h.store.tenantsOpened())

	if _, err := h.service.Me(context.Background(), principalFor(alice, auth.RoleOwner)); err != nil {
		t.Fatalf("Me: %v", err)
	}

	opened := h.store.tenantsOpened()[before:]

	t.Logf("tenant contexts opened by Me: %v", opened)

	if len(opened) != 1 {
		t.Fatalf("Me opened %d tenant contexts, want exactly 1 — it must be one round trip per door", len(opened))
	}

	if opened[0] != alice.OrganizationID {
		t.Errorf("Me opened a context for %s, want the caller's own tenant %s", opened[0], alice.OrganizationID)
	}

	// The organization half still travels the pre-tenant door under its own
	// reason, and the identity half must not have added a second one.
	reasons := h.store.reasonsUsed()

	t.Logf("pre-tenant reasons used: %s", reasons)

	if !strings.Contains(reasons, "list_organizations") {
		t.Errorf("the organization list no longer goes through the pre-tenant door: %s", reasons)
	}
}

// TestMeRefusesWhenTheMembershipBehindTheTokenIsGone is the edge the derived
// policy creates.
//
// A token names an organization; the membership that justified it can be
// revoked before the token expires. The users row then stops being visible in
// that tenant, and the honest answer is "you are not a member" — a 403 — rather
// than a 500 or an identity assembled from the token.
func TestMeRefusesWhenTheMembershipBehindTheTokenIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := registerNamed(t, h, "alice@example.com", "Alice Owner")

	// A token for an organization alice is not in. Same shape as a revoked
	// membership from the query's point of view: no membership joins her row to
	// this tenant, so the policy hides it.
	stale := auth.Principal{
		UserID:    alice.UserID,
		TenantID:  uuid.New(),
		Role:      auth.RoleOwner,
		SessionID: uuid.New(),
	}

	_, err := h.service.Me(context.Background(), stale)

	t.Logf("Me with a token for an organization the caller is not in -> %v", err)

	if !errors.Is(err, auth.ErrNotAMember) {
		t.Fatalf("error = %v, want auth.ErrNotAMember", err)
	}
}
