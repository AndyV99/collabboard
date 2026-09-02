package auth_test

// Renaming a workspace (issue #90).
//
// Until this, the name an organization was created with was the name it had
// forever: `POST /organizations` was the only route under that path and nothing
// anywhere else wrote the column. That was easy to miss while registration was
// the only way to make one — the user types the name and can see what they are
// choosing. #85 made it visible, because the recovery screen has to name a
// workspace for an account whose first attempt was interrupted, under a hint
// that had to promise the choice was permanent.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

func rename(t *testing.T, h *harness, principal auth.Principal, name string) (auth.Organization, error) {
	t.Helper()

	return h.service.RenameOrganization(context.Background(),
		auth.RenameOrganizationInput{Principal: principal, Name: name})
}

func TestAnOwnerCanRenameTheirWorkspace(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")

	renamed, err := rename(t, h, principalFor(founder, auth.RoleOwner), "  Renamed Co  ")
	if err != nil {
		t.Fatalf("RenameOrganization: %v", err)
	}

	if renamed.Name != "Renamed Co" {
		t.Errorf("name = %q, want it trimmed the way every other name is", renamed.Name)
	}

	if renamed.ID != founder.OrganizationID {
		t.Errorf("renamed %s, want the caller's own tenant (%s)", renamed.ID, founder.OrganizationID)
	}

	// And it stuck: the next read reports the new name, not the old one.
	me, err := h.service.Me(context.Background(), principalFor(founder, auth.RoleOwner))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}

	if me.Organizations[0].Name != "Renamed Co" {
		t.Errorf("after the rename the organization list still says %q", me.Organizations[0].Name)
	}
}

// The authorization rule, from every side.
//
// Parity with POST /members, and read the same way: from a tenant-scoped query
// rather than from the token's role claim. A claim is minted at login and
// re-derived at most once per access-token lifetime, so a demoted account
// carries a stale one — which is exactly the case the third subtest drives.
func TestRenamingIsOwnerOrAdminOnly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		role string
		want error
	}{
		{"an owner may", auth.RoleOwner, nil},
		{"an admin may", auth.RoleAdmin, nil},
		{"a member may not", auth.RoleMember, auth.ErrInsufficientRole},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, generousLimits())

			founder := h.register(t, "founder@example.com")

			acting := principalFor(founder, auth.RoleOwner)

			if tc.role != auth.RoleOwner {
				h.register(t, "actor@example.com")

				added, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
					Principal: principalFor(founder, auth.RoleOwner),
					Email:     "actor@example.com",
					Role:      tc.role,
				})
				if err != nil {
					t.Fatalf("seeding the actor as %q: %v", tc.role, err)
				}

				acting = auth.Principal{
					UserID:   added.UserID,
					TenantID: founder.OrganizationID,
					Role:     tc.role,
				}
			}

			_, err := rename(t, h, acting, "Attempted")

			if !errors.Is(err, tc.want) {
				t.Fatalf("RenameOrganization as %q = %v, want %v", tc.role, err, tc.want)
			}
		})
	}
}

// The role in the token is not the role the decision is made against.
func TestAStaleRoleClaimDoesNotGrantARename(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")
	h.register(t, "demoted@example.com")

	added, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: principalFor(founder, auth.RoleOwner),
		Email:     "demoted@example.com",
		Role:      auth.RoleMember,
	})
	if err != nil {
		t.Fatalf("seeding a member: %v", err)
	}

	// A token claiming owner, held by an account whose membership says member.
	// This is what a promotion-then-demotion inside one access-token lifetime
	// leaves behind, and the reason ADR 0008 reads the row rather than the claim.
	forged := auth.Principal{
		UserID:   added.UserID,
		TenantID: founder.OrganizationID,
		Role:     auth.RoleOwner,
	}

	if _, err := rename(t, h, forged, "Should Not Work"); !errors.Is(err, auth.ErrInsufficientRole) {
		t.Fatalf("RenameOrganization with a stale owner claim = %v, want %v",
			err, auth.ErrInsufficientRole)
	}
}

// A token for an organization the caller has been removed from finds no row.
func TestARevokedMembershipCannotRename(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")
	stranger := h.register(t, "stranger@example.com")

	// The stranger's own token, pointed at the founder's tenant. They have a
	// valid session and no membership there — the same shape as a revoked one.
	outsider := auth.Principal{
		UserID:   stranger.UserID,
		TenantID: founder.OrganizationID,
		Role:     auth.RoleOwner,
	}

	if _, err := rename(t, h, outsider, "Not Yours"); !errors.Is(err, auth.ErrNotAMember) {
		t.Fatalf("RenameOrganization from outside the tenant = %v, want %v",
			err, auth.ErrNotAMember)
	}

	// And the founder's workspace is untouched.
	me, err := h.service.Me(context.Background(), principalFor(founder, auth.RoleOwner))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}

	if me.Organizations[0].Name == "Not Yours" {
		t.Fatal("the refused rename landed anyway")
	}
}

// The bound from #67 applies here too, which is the point of that acceptance
// criterion: a rename must not be a way back to the unbounded field.
func TestRenamingIsBoundedByTheSameRuleAsCreation(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")
	acting := principalFor(founder, auth.RoleOwner)

	// 200 runes is the limit, counted in runes -- so 200 emoji fit and 201 do
	// not, exactly as on the create path.
	atTheLimit := strings.Repeat("\U0001F642", 200)

	if _, err := rename(t, h, acting, atTheLimit); err != nil {
		t.Fatalf("renaming to exactly 200 emoji: %v", err)
	}

	for _, name := range []string{atTheLimit + "\U0001F642", strings.Repeat("a", 8000)} {
		if _, err := rename(t, h, acting, name); !errors.Is(err, auth.ErrInvalidInput) {
			t.Errorf("renaming to a %d-rune name = %v, want %v",
				len([]rune(name)), err, auth.ErrInvalidInput)
		}
	}

	for _, blank := range []string{"", "   "} {
		if _, err := rename(t, h, acting, blank); !errors.Is(err, auth.ErrInvalidInput) {
			t.Errorf("renaming to %q = %v, want %v", blank, err, auth.ErrInvalidInput)
		}
	}
}

// The slug is frozen, deliberately.
//
// Regenerating it would make a rename fail whenever another tenant already held
// the slug for that name — a 409 about somebody else's workspace, which is also
// a small existence oracle — and it appears in no URL in this application, so
// there is nothing for a fresh one to fix.
func TestRenamingDoesNotChangeTheSlug(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")

	before := founder.OrganizationSlug

	renamed, err := rename(t, h, principalFor(founder, auth.RoleOwner), "Something Else Entirely")
	if err != nil {
		t.Fatalf("RenameOrganization: %v", err)
	}

	if renamed.Slug != before {
		t.Errorf("slug = %q, want it unchanged at %q", renamed.Slug, before)
	}
}
