package auth_test

// AddMember at the service layer: the role ladder, the duplicate, the unknown
// address, and what a refusal is allowed to say.
//
// The HTTP-layer versions of the cross-tenant claims live in
// internal/api/auth_bola_test.go, which attacks headers, query parameters and
// path segments as well. These are the claims that are about the *decision*
// rather than about the request: who may add, what role they may grant, and
// what the caller learns when the answer is no.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// principalFor is the token an owner of a freshly registered organization would
// be carrying.
func principalFor(result auth.RegisterResult, role string) auth.Principal {
	return auth.Principal{
		UserID:    result.UserID,
		TenantID:  result.OrganizationID,
		Role:      role,
		SessionID: uuid.New(),
	}
}

// TestAddMemberPutsAnExistingAccountIntoTheCallersOrganization is the control
// for everything below: without it, every refusal test would also hold for a
// service that refused everything.
func TestAddMemberPutsAnExistingAccountIntoTheCallersOrganization(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice@example.com")
	bob := h.register(t, "bob@example.com")

	result, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: principalFor(alice, auth.RoleOwner),
		Email:     "BOB@example.com", // capitalised: normalisation has to find the same account
	})
	if err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	t.Logf("added user %s to organization %s as %q", result.UserID, alice.OrganizationID, result.Role)

	if result.UserID != bob.UserID {
		t.Errorf("added user %s, want bob (%s)", result.UserID, bob.UserID)
	}

	if result.Role != auth.RoleMember {
		t.Errorf("role = %q, want %q — an unspecified role must not grant more than the least", result.Role, auth.RoleMember)
	}

	if result.Email != "bob@example.com" {
		t.Errorf("email = %q, want the normalised address", result.Email)
	}

	// The consequence that matters: bob can now act in alice's organization,
	// which is what the demo's second browser needs.
	organizations, err := h.service.Organizations(context.Background(), auth.Principal{UserID: bob.UserID})
	if err != nil {
		t.Fatalf("Organizations(bob): %v", err)
	}

	var joined bool

	for _, org := range organizations {
		t.Logf("bob belongs to %s as %q", org.ID, org.Role)

		if org.ID == alice.OrganizationID {
			joined = true
		}
	}

	if !joined {
		t.Error("bob was added but does not see alice's organization; the membership is not real")
	}
}

// TestOnlyOwnersAndAdminsMayAddAMember is the authorization decision the issue
// insists on making rather than defaulting to "any member".
func TestOnlyOwnersAndAdminsMayAddAMember(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string

		// actorRole is the role the acting user holds in the organization.
		actorRole string

		// granting is the role they ask for; empty means unspecified.
		granting string

		wantErr error
	}{
		{name: "owner adds a member", actorRole: auth.RoleOwner, granting: auth.RoleMember},
		{name: "owner adds an admin", actorRole: auth.RoleOwner, granting: auth.RoleAdmin},
		{name: "admin adds a member", actorRole: auth.RoleAdmin, granting: auth.RoleMember},
		{
			name: "admin cannot manufacture a peer", actorRole: auth.RoleAdmin, granting: auth.RoleAdmin,
			wantErr: auth.ErrInsufficientRole,
		},
		{
			name: "member cannot add anyone", actorRole: auth.RoleMember, granting: auth.RoleMember,
			wantErr: auth.ErrInsufficientRole,
		},
		{
			name: "nobody may grant ownership", actorRole: auth.RoleOwner, granting: auth.RoleOwner,
			wantErr: auth.ErrInvalidInput,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t, generousLimits())

			founder := h.register(t, "founder@example.com")
			actor := h.register(t, "actor@example.com")

			h.register(t, "subject@example.com")

			// The actor holds tc.actorRole in the founder's organization. The
			// founder does the adding, because that is the only way to create a
			// membership — which is itself the property under test.
			if tc.actorRole != auth.RoleOwner {
				if _, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
					Principal: principalFor(founder, auth.RoleOwner),
					Email:     "actor@example.com",
					Role:      tc.actorRole,
				}); err != nil {
					t.Fatalf("seeding the actor as %q: %v", tc.actorRole, err)
				}
			}

			// An owner acts as the founder; anyone else acts as the account
			// just seeded into the founder's organization.
			acting := principalFor(founder, auth.RoleOwner)
			if tc.actorRole != auth.RoleOwner {
				acting = auth.Principal{
					UserID: actor.UserID, TenantID: founder.OrganizationID,
					Role: tc.actorRole, SessionID: uuid.New(),
				}
			}

			result, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
				Principal: acting,
				Email:     "subject@example.com",
				Role:      tc.granting,
			})

			t.Logf("%s granting %q -> %v", tc.actorRole, tc.granting, err)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("AddMember: %v", err)
			}

			if result.Role != tc.granting {
				t.Errorf("role = %q, want %q", result.Role, tc.granting)
			}
		})
	}
}

// TestTheRoleComesFromTheDatabaseAndNotFromTheTokenClaim.
//
// The claim is minted at login and refreshed at most once per access-token
// lifetime, so a token can outlive the privilege it names. This asserts the
// service reads `memberships` instead of believing the claim — in both
// directions, because only checking the demotion case would also pass for an
// implementation that refused everything.
func TestTheRoleComesFromTheDatabaseAndNotFromTheTokenClaim(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")
	plain := h.register(t, "plain@example.com")
	h.register(t, "subject@example.com")

	if _, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: principalFor(founder, auth.RoleOwner),
		Email:     "plain@example.com",
	}); err != nil {
		t.Fatalf("seeding a plain member: %v", err)
	}

	// A token that claims "owner" for a user the database records as a member.
	inflated := auth.Principal{
		UserID: plain.UserID, TenantID: founder.OrganizationID, Role: auth.RoleOwner, SessionID: uuid.New(),
	}

	_, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: inflated, Email: "subject@example.com",
	})

	t.Logf("a token claiming owner for a database member -> %v", err)

	if !errors.Is(err, auth.ErrInsufficientRole) {
		t.Fatalf("error = %v, want ErrInsufficientRole; the role claim was believed over the database", err)
	}

	// The other direction: a token whose role claim says "member" for a user
	// the database records as the owner. If the claim were being read, this
	// would be refused too, and the assertion above would prove nothing about
	// where the role came from.
	deflated := auth.Principal{
		UserID: founder.UserID, TenantID: founder.OrganizationID, Role: auth.RoleMember, SessionID: uuid.New(),
	}

	if _, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: deflated, Email: "subject@example.com",
	}); err != nil {
		t.Fatalf("a token claiming member for the database owner was refused (%v); the claim is being read, not the row", err)
	}

	t.Log("confirmed: the decision follows the memberships row in both directions, not the token")
}

// TestAddingSomeoneAlreadyInTheOrganizationIsACleanConflict.
//
// The refusal comes from the unique index rather than from a prior SELECT, so
// there is no window in which two concurrent requests both see no row.
func TestAddingSomeoneAlreadyInTheOrganizationIsACleanConflict(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice@example.com")
	h.register(t, "bob@example.com")

	owner := principalFor(alice, auth.RoleOwner)

	if _, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: owner, Email: "bob@example.com",
	}); err != nil {
		t.Fatalf("the first addition failed: %v", err)
	}

	_, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: owner, Email: "bob@example.com", Role: auth.RoleAdmin,
	})

	t.Logf("adding the same account a second time -> %v", err)

	if !errors.Is(err, auth.ErrAlreadyMember) {
		t.Fatalf("error = %v, want ErrAlreadyMember", err)
	}

	// And the second attempt must not have upgraded the existing membership as
	// a side effect: the conflict is a refusal, not an upsert.
	organizations, err := h.service.Organizations(context.Background(), auth.Principal{UserID: bobID(t, h)})
	if err != nil {
		t.Fatalf("Organizations(bob): %v", err)
	}

	for _, org := range organizations {
		if org.ID == alice.OrganizationID && org.Role != auth.RoleMember {
			t.Errorf("the refused second addition changed the role to %q", org.Role)
		}
	}
}

// TestAnUnknownAddressIsRefusedWithoutDisclosingAnythingElse is the
// enumeration claim at the service layer.
//
// What is asserted: the error is one sentinel with no per-address content, the
// same one for every unregistered address, and the log line the refusal
// produces names the actor and the tenant but never the address that was
// probed. The HTTP-status half of the claim — that 404 tells the caller the
// address has no account, and that POST /auth/register already tells an
// anonymous caller the same thing — is demonstrated in
// members_integration_test.go.
func TestAnUnknownAddressIsRefusedWithoutDisclosingAnythingElse(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice@example.com")
	owner := principalFor(alice, auth.RoleOwner)

	probes := []string{
		"nobody-1@example.com",
		"nobody-2@example.com",
		"ceo@some-other-company.example",
	}

	var messages []string

	for _, probe := range probes {
		_, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
			Principal: owner, Email: probe,
		})
		if !errors.Is(err, auth.ErrNoSuchAccount) {
			t.Fatalf("AddMember(%s) = %v, want ErrNoSuchAccount", probe, err)
		}

		messages = append(messages, err.Error())

		if strings.Contains(err.Error(), probe) {
			t.Errorf("the error quotes the probed address back: %q", err.Error())
		}
	}

	t.Logf("three unregistered addresses produced: %q", messages)

	for _, message := range messages[1:] {
		if message != messages[0] {
			t.Errorf("two unregistered addresses produced different errors (%q, %q); the difference is content", messages[0], message)
		}
	}

	logs := h.logs.String()

	for _, probe := range probes {
		if strings.Contains(logs, probe) {
			t.Errorf("the probed address %q reached the logs; a log of refusals must not become the address list", probe)
		}
	}

	// The other half: the refusal *is* recorded, with enough to see a burst.
	for _, want := range []string{"auth.member.add_refused", "no_such_account", alice.UserID.String(), alice.OrganizationID.String()} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs do not contain %q; bulk probing would leave no trace", want)
		}
	}
}

// TestAnUnauthorizedCallerNeverReachesTheDirectory.
//
// Authorization is decided before the address is looked up, so a caller who may
// not add never causes a pre-tenant lookup. The pre-tenant path is audited by
// reason (ADR 0002), and the absence of an invite_lookup is the assertion.
func TestAnUnauthorizedCallerNeverReachesTheDirectory(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	founder := h.register(t, "founder@example.com")
	plain := h.register(t, "plain@example.com")
	h.register(t, "subject@example.com")

	if _, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: principalFor(founder, auth.RoleOwner), Email: "plain@example.com",
	}); err != nil {
		t.Fatalf("seeding a plain member: %v", err)
	}

	before := strings.Count(h.store.reasonsUsed(), "invite_lookup")

	_, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: auth.Principal{
			UserID: plain.UserID, TenantID: founder.OrganizationID, Role: auth.RoleMember, SessionID: uuid.New(),
		},
		Email: "subject@example.com",
	})
	if !errors.Is(err, auth.ErrInsufficientRole) {
		t.Fatalf("error = %v, want ErrInsufficientRole", err)
	}

	after := strings.Count(h.store.reasonsUsed(), "invite_lookup")

	t.Logf("pre-tenant reasons after the refused attempt: %s", h.store.reasonsUsed())

	if after != before {
		t.Errorf("a refused caller caused %d directory lookup(s); authorization must be decided first", after-before)
	}
}

// TestAddMemberRefusesACallerWhoIsNoLongerAMember.
//
// A signed access token outlives a revoked membership by up to its own
// lifetime. The tenant transaction finds no membership row and the addition is
// refused, rather than the token's role claim carrying the decision.
func TestAddMemberRefusesACallerWhoIsNoLongerAMember(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice@example.com")
	h.register(t, "bob@example.com")

	stranger := auth.Principal{
		UserID: uuid.New(), TenantID: alice.OrganizationID, Role: auth.RoleOwner, SessionID: uuid.New(),
	}

	_, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
		Principal: stranger, Email: "bob@example.com",
	})

	t.Logf("a token naming a tenant the subject has no membership in -> %v", err)

	if !errors.Is(err, auth.ErrNotAMember) {
		t.Fatalf("error = %v, want ErrNotAMember", err)
	}
}

// TestAddMemberRejectsMalformedInput.
func TestAddMemberRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	h := newHarness(t, generousLimits())

	alice := h.register(t, "alice@example.com")
	owner := principalFor(alice, auth.RoleOwner)

	for _, tc := range []struct{ name, email, role string }{
		{name: "empty address", email: "   "},
		{name: "not an address", email: "not-an-address"},
		{name: "an address longer than RFC 5321 allows", email: strings.Repeat("a", 250) + "@example.com"},
		{name: "an unknown role", email: "alice@example.com", role: "superuser"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.service.AddMember(context.Background(), auth.AddMemberInput{
				Principal: owner, Email: tc.email, Role: tc.role,
			})

			t.Logf("%s -> %v", tc.name, err)

			if !errors.Is(err, auth.ErrInvalidInput) {
				t.Errorf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

// bobID reads bob's user id back out of the fake by logging in, which is the
// only route the service exposes. Cheaper than threading the registration
// result through every helper.
func bobID(t *testing.T, h *harness) uuid.UUID {
	t.Helper()

	result, err := h.service.Login(context.Background(), auth.LoginInput{
		Email: "bob@example.com", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("login as bob: %v", err)
	}

	return result.Principal.UserID
}
