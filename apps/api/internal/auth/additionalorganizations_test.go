package auth_test

// Creating a second workspace (issue #86).
//
// #34 argued for POST /organizations over a self-healing login on the grounds
// that "an account should be able to have several workspaces — so the repair
// path falls out of a feature rather than being one". The repair path shipped;
// the feature is this.
//
// What was missing was never the machinery — provisionOrganization has always
// created an organization plus its owner membership for any subject, and
// SwitchOrganization has always handled an account belonging to several. What
// was missing was an answer to the authorization question, and these tests are
// where that answer is written down: **any authenticated account, up to five
// owned**.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// maxOwned mirrors maxOwnedOrganizations, which is unexported.
//
// Written out rather than exported for a test: the cap is part of the contract
// this endpoint makes with its callers, and a test that read the number out of
// the package could not fail when the package changed it.
const maxOwned = 5

func createAdditional(t *testing.T, h *harness, userID uuid.UUID, name string) (auth.CreateOrganizationResult, error) {
	t.Helper()

	return h.service.CreateAdditionalOrganization(context.Background(), userID,
		auth.CreateAdditionalOrganizationInput{Name: name})
}

func TestAnAccountCanCreateASecondWorkspaceAndOwnsIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")

	created, err := createAdditional(t, h, founder.UserID, "Side Project")
	if err != nil {
		t.Fatalf("CreateAdditionalOrganization: %v", err)
	}

	if created.UserID != founder.UserID {
		t.Errorf("created for %s, want the caller (%s)", created.UserID, founder.UserID)
	}

	if created.OrganizationID == founder.OrganizationID {
		t.Fatal("the second workspace has the first one's id; nothing new was created")
	}

	// Owner, not member. A workspace you created and cannot administer would be
	// a worse outcome than not being able to create one.
	if created.Role != auth.RoleOwner {
		t.Errorf("role = %q, want %q", created.Role, auth.RoleOwner)
	}

	if created.OrganizationName != "Side Project" {
		t.Errorf("name = %q, want %q", created.OrganizationName, "Side Project")
	}

	// And the account now belongs to both, which is what makes the existing
	// org switcher meaningful rather than decorative.
	me, err := h.service.Me(context.Background(), principalFor(founder, auth.RoleOwner))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}

	if len(me.Organizations) != 2 {
		t.Fatalf("the account belongs to %d organizations, want 2", len(me.Organizations))
	}
}

// The cap, from both sides of the boundary.
func TestTheOwnedWorkspaceCapIsEnforced(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	// Registration is the first owned workspace, so this loop reaches the cap.
	founder := h.register(t, "prolific@example.com")

	for i := 2; i <= maxOwned; i++ {
		if _, err := createAdditional(t, h, founder.UserID, fmt.Sprintf("Workspace %d", i)); err != nil {
			t.Fatalf("creating workspace %d of %d: %v", i, maxOwned, err)
		}
	}

	// Exactly at the cap is the last one that works, not the first that fails.
	_, err := createAdditional(t, h, founder.UserID, "One Too Many")
	if !errors.Is(err, auth.ErrTooManyOrganizations) {
		t.Fatalf("CreateAdditionalOrganization at the cap = %v, want %v",
			err, auth.ErrTooManyOrganizations)
	}

	me, err := h.service.Me(context.Background(), principalFor(founder, auth.RoleOwner))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}

	if len(me.Organizations) != maxOwned {
		t.Errorf("the refused call left the account with %d organizations, want %d",
			len(me.Organizations), maxOwned)
	}
}

// A membership somebody else granted must not consume the budget.
//
// This is the half of the rule that is easy to get wrong by counting the wrong
// list. Counting *memberships* would mean being invited into five colleagues'
// workspaces makes you unable to start your own — and would hand every admin a
// way to exhaust another account's quota by adding them to things.
func TestBeingAMemberOfOtherWorkspacesDoesNotConsumeTheCap(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	guest := h.register(t, "guest@example.com")

	// Five other people's workspaces, with the guest added to each. That is
	// already at the cap if the rule counted memberships.
	for i := range maxOwned {
		host := h.register(t, fmt.Sprintf("host%d@example.com", i))

		if _, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
			Principal: principalFor(host, auth.RoleOwner),
			Email:     "guest@example.com",
		}); err != nil {
			t.Fatalf("seeding membership %d: %v", i, err)
		}
	}

	me, err := h.service.Me(context.Background(), principalFor(guest, auth.RoleOwner))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}

	if len(me.Organizations) != maxOwned+1 {
		t.Fatalf("the guest belongs to %d organizations, want %d; the fixture is not what this test needs",
			len(me.Organizations), maxOwned+1)
	}

	// Six memberships, one of them owned. Four more creations must still work.
	for i := 2; i <= maxOwned; i++ {
		if _, err := createAdditional(t, h, guest.UserID, fmt.Sprintf("Mine %d", i)); err != nil {
			t.Fatalf("creating owned workspace %d while a member of %d others: %v",
				i, maxOwned, err)
		}
	}

	// And the cap still applies to the ones they own.
	if _, err := createAdditional(t, h, guest.UserID, "One Too Many"); !errors.Is(err, auth.ErrTooManyOrganizations) {
		t.Errorf("CreateAdditionalOrganization = %v, want %v", err, auth.ErrTooManyOrganizations)
	}
}

func TestTheWorkspaceNameIsRequiredRatherThanDefaulted(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "namer@example.com")

	for _, name := range []string{"", "   ", "\t\n"} {
		_, err := createAdditional(t, h, founder.UserID, name)
		if !errors.Is(err, auth.ErrInvalidInput) {
			t.Errorf("CreateAdditionalOrganization(%q) = %v, want %v",
				name, err, auth.ErrInvalidInput)
		}
	}

	// The default that registration applies -- "<display name>'s workspace" --
	// exists because a person who was never asked still needs a workspace.
	// Somebody deliberately creating a second one has been asked, and a second
	// "Test Person's workspace" beside the first is not a kindness.
	created, err := createAdditional(t, h, founder.UserID, "  Trimmed  ")
	if err != nil {
		t.Fatalf("CreateAdditionalOrganization: %v", err)
	}

	if created.OrganizationName != "Trimmed" {
		t.Errorf("name = %q, want it trimmed the way every other name is", created.OrganizationName)
	}

	if strings.Contains(created.OrganizationName, "workspace") {
		t.Errorf("name = %q; the generated default leaked into a named workspace",
			created.OrganizationName)
	}
}

// The subject is the argument, and there is nowhere else it could come from.
//
// [auth.CreateAdditionalOrganizationInput] has one field and it is the name.
// This asserts the consequence: two accounts calling with identical input get
// their own workspaces, and neither can reach the other's.
func TestTheWorkspaceIsCreatedForTheCallerAndNobodyElse(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice@example.com")
	bob := h.register(t, "bob@example.com")

	forAlice, err := createAdditional(t, h, alice.UserID, "Shared Name")
	if err != nil {
		t.Fatalf("CreateAdditionalOrganization for alice: %v", err)
	}

	forBob, err := createAdditional(t, h, bob.UserID, "Shared Name")
	if err != nil {
		t.Fatalf("CreateAdditionalOrganization for bob: %v", err)
	}

	if forAlice.OrganizationID == forBob.OrganizationID {
		t.Fatal("the same organization was returned to two accounts")
	}

	if forAlice.UserID != alice.UserID || forBob.UserID != bob.UserID {
		t.Fatalf("workspaces landed on the wrong subjects: alice got %s, bob got %s",
			forAlice.UserID, forBob.UserID)
	}

	// Alice's list must not have grown Bob's workspace in it.
	aliceOrgs, err := h.service.Me(context.Background(), principalFor(alice, auth.RoleOwner))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}

	for _, organization := range aliceOrgs.Organizations {
		if organization.ID == forBob.OrganizationID {
			t.Fatal("bob's workspace appears in alice's organizations")
		}
	}
}
