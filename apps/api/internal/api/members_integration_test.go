//go:build integration

package api_test

// Adding a member, end to end, against a real Postgres with the real policies
// (issue #61).
//
// The unit tests in internal/auth own the role ladder and the BOLA suite in
// internal/api owns "which tenant did the write land in". This file owns the
// two claims that only a real database can settle:
//
//  1. **the demo actually works.** Two accounts, one organization, and the
//     second one reads the first one's boards through the ordinary
//     tenant-scoped path — no fixture, no seeding, no owner pool. That is the
//     third step of "sign up → create board → invite teammate → move a card and
//     watch it in a second browser", and until this test existed the project's
//     headline feature could not be demonstrated with two real users;
//  2. **what an attacker learns.** The enumeration behaviour is exercised
//     rather than described: real probes against a real directory, with the
//     responses compared byte for byte and the server's own log inspected for
//     the addresses that were probed.

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// addMember posts an addition and returns the response.
func addMember(t *testing.T, s *server, token, email, role string) response {
	t.Helper()

	body := map[string]string{"email": email}
	if role != "" {
		body["role"] = role
	}

	return s.do(t, http.MethodPost, "/api/v1/members", token, body)
}

// TestTwoAccountsOneOrganizationSeeTheSameBoard is acceptance criterion 1.
//
// Everything goes over HTTP through the real endpoints, in the order a person
// would: alice registers, builds a board, adds bob by address; bob switches
// into her organization and lists what is there.
func TestTwoAccountsOneOrganizationSeeTheSameBoard(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	t.Logf("alice: user=%s tenant=%s", alice.userID, alice.tenantID)
	t.Logf("bob:   user=%s tenant=%s", bob.userID, bob.tenantID)

	board := build(t, s, alice, "shared", "write the ADR", "ship it")

	t.Logf("alice built project=%s board=%s", board.project, board.board)

	// Before the addition, bob cannot switch into alice's organization at all.
	// Without this the test below would also pass if bob had somehow been a
	// member from the start.
	before := s.do(t, http.MethodPost, "/api/v1/auth/organization", bob.accessToken,
		map[string]string{"organization_id": alice.tenantID.String()})

	t.Logf("bob switching into alice's organization *before* the addition -> %d %s", before.status, before.raw)

	if before.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — bob is not a member yet", before.status)
	}

	added := addMember(t, s, alice.accessToken, bob.email, "")

	t.Logf("alice adding %s -> %d %s", bob.email, added.status, added.raw)

	if added.status != http.StatusCreated {
		t.Fatalf("adding a member: status %d, body %s", added.status, added.raw)
	}

	member, ok := added.body["member"].(map[string]any)
	if !ok {
		t.Fatalf("no member in the response: %s", added.raw)
	}

	if member["user_id"] != bob.userID.String() {
		t.Errorf("member user_id = %v, want bob (%s)", member["user_id"], bob.userID)
	}

	if member["role"] != "member" {
		t.Errorf("role = %v, want \"member\" — an unspecified role must grant the least", member["role"])
	}

	// The response must not carry anything read out of the global directory.
	if _, leaked := member["display_name"]; leaked {
		t.Errorf("the addition response carries a display name read from the directory: %s", added.raw)
	}

	// Bob switches. This is the ordinary, membership-checked path — the same
	// endpoint auth_bola_test.go attacks — and it now says yes.
	switched := s.do(t, http.MethodPost, "/api/v1/auth/organization", bob.accessToken,
		map[string]string{"organization_id": alice.tenantID.String()})

	t.Logf("bob switching into alice's organization *after* the addition -> %d", switched.status)

	if switched.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", switched.status, switched.raw)
	}

	bobInAlicesOrg := stringField(t, switched.body, "access_token")

	// The payoff: bob reads alice's project, board and cards through the
	// ordinary tenant-scoped path. No fixture and no owner pool — the rows come
	// back because row-level security lets them, which it does because a
	// membership now joins bob to alice's tenant.
	projects := s.do(t, http.MethodGet, "/api/v1/projects", bobInAlicesOrg, nil)

	t.Logf("bob, GET /projects in alice's organization -> %d %s", projects.status, projects.raw)

	if projects.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", projects.status, projects.raw)
	}

	if !strings.Contains(projects.raw, board.project.String()) {
		t.Fatalf("bob cannot see alice's project %s: %s", board.project, projects.raw)
	}

	boards := s.do(t, http.MethodGet, "/api/v1/projects/"+board.project.String()+"/boards", bobInAlicesOrg, nil)

	t.Logf("bob, GET /projects/:id/boards -> %d %s", boards.status, boards.raw)

	if !strings.Contains(boards.raw, board.board.String()) {
		t.Fatalf("bob cannot see alice's board %s: %s", board.board, boards.raw)
	}

	cards := s.do(t, http.MethodGet, "/api/v1/boards/"+board.board.String()+"/cards", bobInAlicesOrg, nil)

	t.Logf("bob, GET /boards/:id/cards -> %d %s", cards.status, cards.raw)

	for _, title := range []string{"write the ADR", "ship it"} {
		if !strings.Contains(cards.raw, title) {
			t.Errorf("bob cannot see the card %q: %s", title, cards.raw)
		}
	}

	// And both accounts now appear in one member list, which is what the web
	// app renders.
	members := s.do(t, http.MethodGet, "/api/v1/members", bobInAlicesOrg, nil)

	t.Logf("bob, GET /members -> %d %s", members.status, members.raw)

	for _, email := range []string{alice.email, bob.email} {
		if !bytes.Contains([]byte(members.raw), []byte(email)) {
			t.Errorf("the member list is missing %s: %s", email, members.raw)
		}
	}

	// Bob's original organization is untouched: he is in two now, not moved
	// between them.
	me := s.do(t, http.MethodGet, "/api/v1/me", bobInAlicesOrg, nil)

	t.Logf("bob, GET /me -> %d %s", me.status, me.raw)

	for _, tenantID := range []uuid.UUID{alice.tenantID, bob.tenantID} {
		if !strings.Contains(me.raw, tenantID.String()) {
			t.Errorf("/me does not list %s among bob's organizations: %s", tenantID, me.raw)
		}
	}
}

// TestAddingSomeoneWhoIsAlreadyAMemberIsACleanConflict.
//
// The refusal comes from the unique index on (tenant_id, user_id), so this also
// asserts the thing the issue actually cares about: no second membership row.
func TestAddingSomeoneWhoIsAlreadyAMemberIsACleanConflict(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	if resp := addMember(t, s, alice.accessToken, bob.email, ""); resp.status != http.StatusCreated {
		t.Fatalf("the first addition: status %d, body %s", resp.status, resp.raw)
	}

	second := addMember(t, s, alice.accessToken, bob.email, "admin")

	t.Logf("adding the same account again, asking for a higher role -> %d %s", second.status, second.raw)

	if second.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", second.status)
	}

	// Exactly one row, and it still says "member". A conflict that silently
	// upgraded the existing membership would be a privilege change disguised as
	// an error.
	members := s.do(t, http.MethodGet, "/api/v1/members", alice.accessToken, nil)

	t.Logf("the member list afterwards -> %d %s", members.status, members.raw)

	if got := strings.Count(members.raw, bob.email); got != 1 {
		t.Errorf("bob appears %d times in the member list, want exactly 1: %s", got, members.raw)
	}

	if strings.Contains(members.raw, `"role":"admin"`) {
		t.Errorf("the refused second addition changed a role: %s", members.raw)
	}
}

// TestWhoMayAddAMember is the authorization decision, against the database that
// stores the roles.
//
// The service-layer version in internal/auth uses a fake; this one uses real
// memberships rows read back through the real policy, which is the half that
// would break if GetMembership were ever scoped wrongly.
func TestWhoMayAddAMember(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")
	carol := s.register(t, "carol")
	dave := s.register(t, "dave")

	// alice (owner) makes bob an admin and carol a plain member.
	if resp := addMember(t, s, alice.accessToken, bob.email, "admin"); resp.status != http.StatusCreated {
		t.Fatalf("owner adding an admin: status %d, body %s", resp.status, resp.raw)
	}

	if resp := addMember(t, s, alice.accessToken, carol.email, "member"); resp.status != http.StatusCreated {
		t.Fatalf("owner adding a member: status %d, body %s", resp.status, resp.raw)
	}

	tokenIn := func(t *testing.T, acct account) string {
		t.Helper()

		switched := s.do(t, http.MethodPost, "/api/v1/auth/organization", acct.accessToken,
			map[string]string{"organization_id": alice.tenantID.String()})
		if switched.status != http.StatusOK {
			t.Fatalf("switching %s into alice's organization: status %d, body %s", acct.email, switched.status, switched.raw)
		}

		return stringField(t, switched.body, "access_token")
	}

	adminToken := tokenIn(t, bob)
	memberToken := tokenIn(t, carol)

	t.Run("an admin may add a member", func(t *testing.T) {
		resp := addMember(t, s, adminToken, dave.email, "member")

		t.Logf("admin adding a member -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusCreated {
			t.Errorf("status = %d, want 201: %s", resp.status, resp.raw)
		}
	})

	t.Run("an admin may not add another admin", func(t *testing.T) {
		// dave is already in by now, so this would be a 409 if the role check
		// did not fire first — which is itself the assertion.
		resp := addMember(t, s, adminToken, dave.email, "admin")

		t.Logf("admin adding an admin -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusForbidden {
			t.Errorf("status = %d, want 403: %s", resp.status, resp.raw)
		}
	})

	t.Run("a plain member may not add anyone", func(t *testing.T) {
		resp := addMember(t, s, memberToken, alice.email, "member")

		t.Logf("member adding anyone -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusForbidden {
			t.Errorf("status = %d, want 403: %s", resp.status, resp.raw)
		}
	})

	t.Run("nobody may grant ownership", func(t *testing.T) {
		resp := addMember(t, s, alice.accessToken, dave.email, "owner")

		t.Logf("owner granting ownership -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusBadRequest {
			t.Errorf("status = %d, want 400: %s", resp.status, resp.raw)
		}
	})

	// The control: the organization has exactly the members it should, and
	// nothing a refusal was supposed to prevent got in.
	members := s.do(t, http.MethodGet, "/api/v1/members", alice.accessToken, nil)

	t.Logf("alice's organization afterwards -> %s", members.raw)

	if strings.Count(members.raw, `"role":"owner"`) != 1 {
		t.Errorf("the organization has more than one owner: %s", members.raw)
	}

	if strings.Count(members.raw, `"role":"admin"`) != 1 {
		t.Errorf("the organization has %d admins, want 1: %s", strings.Count(members.raw, `"role":"admin"`), members.raw)
	}
}

// TestAddingAMemberCannotCrossTheTenantBoundary is the integration half of the
// BOLA claim.
//
// auth_bola_test.go proves against a recording fake that no foreign tenant
// context is opened. Here the assertion is the one a customer would make:
// alice's organization is unchanged after bob tries every channel, and bob's is
// unchanged after alice does.
func TestAddingAMemberCannotCrossTheTenantBoundary(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	// One target account per attack. Reusing a single account would make every
	// attack after the first a duplicate — a 409 that says nothing about where
	// the addition would have gone, which is the thing being tested.
	targets := []struct {
		name   string
		path   string
		header string
		extra  map[string]string
	}{
		{name: "X-Organization-ID header", path: "/api/v1/members", header: "X-Organization-ID"},
		{name: "X-Tenant-ID header", path: "/api/v1/members", header: "X-Tenant-ID"},
		{
			name: "an organization_id field in the body", path: "/api/v1/members",
			extra: map[string]string{"organization_id": bob.tenantID.String(), "tenant_id": bob.tenantID.String()},
		},
		{name: "an org query parameter", path: "/api/v1/members?org=" + bob.tenantID.String()},
		{
			name: "an organization in the path",
			path: "/api/v1/organizations/" + bob.tenantID.String() + "/members",
		},
		// The control, run through the same loop so it is impossible to change
		// the attacks without changing it too: no injection at all.
		{name: "control: no injection", path: "/api/v1/members"},
	}

	for index, attack := range targets {
		t.Run(attack.name, func(t *testing.T) {
			target := s.register(t, fmt.Sprintf("target%d", index))

			body := map[string]string{"email": target.email}
			for key, value := range attack.extra {
				body[key] = value
			}

			req := s.request(t, http.MethodPost, attack.path, alice.accessToken, body)
			if attack.header != "" {
				req.Header.Set(attack.header, bob.tenantID.String())
			}

			resp := s.send(t, req)

			t.Logf("alice, %s -> %d %s", attack.name, resp.status, resp.raw)

			// Whatever happened, bob's organization must still be exactly bob.
			bobsMembers := s.do(t, http.MethodGet, "/api/v1/members", bob.accessToken, nil)

			t.Logf("bob's member list afterwards -> %s", bobsMembers.raw)

			if strings.Contains(bobsMembers.raw, target.email) {
				t.Errorf("BOLA: alice added %s to bob's organization through %s\n%s",
					target.email, attack.name, bobsMembers.raw)
			}

			// And the other half, which is what stops all of this from holding
			// against an endpoint that simply refuses: an addition the router
			// accepted landed in *alice's* organization.
			if resp.status != http.StatusCreated {
				return
			}

			alicesMembers := s.do(t, http.MethodGet, "/api/v1/members", alice.accessToken, nil)

			if !strings.Contains(alicesMembers.raw, target.email) {
				t.Errorf("the addition answered 201 but the account is in neither organization: %s", alicesMembers.raw)
			}
		})
	}
}

// TestWhatAnAttackerLearnsByAddingAddressesInBulk is the enumeration behaviour,
// demonstrated.
//
// The claim being tested is not "nothing is disclosed" — that would be false.
// It is:
//
//   - the caller learns exactly one bit per request, "this address has an
//     account", and learns it only from the status;
//   - the refusal body is byte-identical for every unregistered address, and
//     carries no user id, no display name and nothing about any organization;
//   - a registered address that is *not* in the caller's organization
//     discloses nothing more than that same bit — in particular, no
//     information about the organizations it is in;
//   - POST /api/v1/auth/register already gives an *unauthenticated* caller the
//     same bit, so this endpoint is not the widest way to ask;
//   - the probing is recorded, and the record does not contain the addresses.
func TestWhatAnAttackerLearnsByAddingAddressesInBulk(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	victim := s.register(t, "victim")

	probes := []string{
		"nobody-" + uuid.NewString() + "@example.com",
		"nobody-" + uuid.NewString() + "@example.com",
		"ceo-" + uuid.NewString() + "@some-other-company.example",
	}

	var refusals []string

	for _, probe := range probes {
		resp := addMember(t, s, alice.accessToken, probe, "")

		t.Logf("probing an unregistered address -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.status)
		}

		refusals = append(refusals, resp.raw)

		if strings.Contains(resp.raw, probe) {
			t.Errorf("the refusal quotes the probed address back: %s", resp.raw)
		}
	}

	for _, refusal := range refusals[1:] {
		if refusal != refusals[0] {
			t.Errorf("two unregistered addresses answered differently:\n%s\n%s", refusals[0], refusal)
		}
	}

	t.Logf("every unregistered address answered 404 %s", refusals[0])

	// A registered address that alice has no relationship with. The status
	// differs — that is the one bit — and the body must not contain anything
	// beyond the membership alice just created.
	hit := addMember(t, s, alice.accessToken, victim.email, "")

	t.Logf("probing a registered address -> %d %s", hit.status, hit.raw)

	if hit.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", hit.status)
	}

	if strings.Contains(hit.raw, victim.tenantID.String()) {
		t.Errorf("the response names an organization the added account belongs to elsewhere: %s", hit.raw)
	}

	if strings.Contains(hit.raw, "victim person") {
		t.Errorf("the response carries the added account's display name: %s", hit.raw)
	}

	// The same bit, from an endpoint that needs no credential at all. This is
	// why the 404 above is an accepted trade rather than a new hole: the
	// narrower channel cannot be the weakest one.
	duplicate := s.do(t, http.MethodPost, "/api/v1/auth/register", "", map[string]string{
		"email": victim.email, "password": integrationPass, "display_name": "Impostor",
	})

	t.Logf("the same question asked anonymously, via POST /auth/register -> %d %s", duplicate.status, duplicate.raw)

	if duplicate.status != http.StatusConflict {
		t.Errorf("register answered %d for a taken address; the comparison this test rests on no longer holds", duplicate.status)
	}

	// And the probing left a trail that does not contain the probes.
	logs := s.logs.String()

	for _, probe := range probes {
		if strings.Contains(logs, probe) {
			t.Errorf("a probed address reached the server log: %q", probe)
		}
	}

	for _, want := range []string{"auth.member.add_refused", "no_such_account", alice.userID.String(), alice.tenantID.String()} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs do not contain %q, so bulk probing would leave no trace", want)
		}
	}

	t.Logf("%d refusals recorded under auth.member.add_refused with the actor and tenant, and none with the address",
		strings.Count(logs, "no_such_account"))
}

// TestAddingAMemberRejectsMalformedInput covers the statuses a client has to be
// able to tell apart.
func TestAddingAMemberRejectsMalformedInput(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")

	for _, tc := range []struct {
		name string
		body map[string]string
		want int
	}{
		{name: "no email at all", body: map[string]string{"role": "member"}, want: http.StatusBadRequest},
		{name: "an empty email", body: map[string]string{"email": "   "}, want: http.StatusBadRequest},
		{name: "not an address", body: map[string]string{"email": "not-an-address"}, want: http.StatusBadRequest},
		{
			name: "an unknown role",
			body: map[string]string{"email": alice.email, "role": "superuser"},
			want: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := s.do(t, http.MethodPost, "/api/v1/members", alice.accessToken, tc.body)

			t.Logf("%s -> %d %s", tc.name, resp.status, resp.raw)

			if resp.status != tc.want {
				t.Errorf("status = %d, want %d: %s", resp.status, tc.want, resp.raw)
			}
		})
	}

	t.Run("no token", func(t *testing.T) {
		resp := s.do(t, http.MethodPost, "/api/v1/members", "", map[string]string{"email": alice.email})

		t.Logf("unauthenticated -> %d %s", resp.status, resp.raw)

		if resp.status != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.status)
		}
	})
}
