//go:build integration

package api_test

// GET /me reports the caller's own identity, against a real Postgres with the
// real policies (issue #75).
//
// The claim the unit tests cannot settle is the one this file exists for: the
// caller's own `users` row really is visible from inside their own tenant
// context. `users` has no tenant_id and its policy is *derived* —
// users_visible_via_membership makes a row visible only when a membership joins
// it to the current tenant — so "a tenant-scoped read can see the caller" is a
// statement about a policy, and the only honest way to check it is to ask
// Postgres.
//
// Two accounts in one organization, because that is where the interesting way
// to be wrong lives: a query that ignored the user id, or a handler that read
// the first row it could see, would hand each of them the other's name.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// decodeInto decodes a response into a typed struct, failing the test rather
// than leaving a zero value that reads like a wrong answer. The shared `do`
// helper decodes into a map[string]any, which is right for probing a shape and
// wrong for asserting one.
func decodeInto(t *testing.T, resp response, target any) {
	t.Helper()

	if err := json.Unmarshal([]byte(resp.raw), target); err != nil {
		t.Fatalf("decoding the response: %v (%s)", err, resp.raw)
	}
}

// meBody is the decoded /me response, named so a shape regression fails at the
// decode rather than three assertions later.
type meBody struct {
	UserID        string           `json:"user_id"`
	Email         string           `json:"email"`
	DisplayName   string           `json:"display_name"`
	Role          string           `json:"role"`
	SessionID     string           `json:"session_id"`
	Organization  map[string]any   `json:"organization"`
	Organizations []map[string]any `json:"organizations"`
}

func getMe(t *testing.T, s *server, token, path string) (response, meBody) {
	t.Helper()

	resp := s.do(t, http.MethodGet, path, token, nil)

	var body meBody

	if resp.status == http.StatusOK {
		decodeInto(t, resp, &body)
	}

	return resp, body
}

// TestMeReportsEachCallersOwnIdentity is the acceptance criterion: two accounts
// in one organization, each seeing themselves and not the other.
func TestMeReportsEachCallersOwnIdentity(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	t.Logf("alice: user=%s tenant=%s email=%s", alice.userID, alice.tenantID, alice.email)
	t.Logf("bob:   user=%s tenant=%s email=%s", bob.userID, bob.tenantID, bob.email)

	// One organization. Registration gives each of them their own, so without
	// this the two callers would be in different tenants and "each sees their
	// own" would also hold for a query that returned the only row it could see.
	added := addMember(t, s, alice.accessToken, bob.email, "")
	if added.status != http.StatusCreated {
		t.Fatalf("adding bob to alice's organization: %d %s", added.status, added.raw)
	}

	switched := s.do(t, http.MethodPost, "/api/v1/auth/organization", bob.accessToken,
		map[string]string{"organization_id": alice.tenantID.String()})
	if switched.status != http.StatusOK {
		t.Fatalf("bob switching into alice's organization: %d %s", switched.status, switched.raw)
	}

	bobInAlicesOrg := stringField(t, switched.body, "access_token")

	for _, subject := range []struct {
		name       string
		token      string
		wantUserID string
		wantEmail  string
		wantName   string
		otherEmail string
	}{
		{
			name:       "alice",
			token:      alice.accessToken,
			wantUserID: alice.userID.String(),
			wantEmail:  alice.email,
			wantName:   "alice person",
			otherEmail: bob.email,
		},
		{
			name:       "bob, holding a token for alice's organization",
			token:      bobInAlicesOrg,
			wantUserID: bob.userID.String(),
			wantEmail:  bob.email,
			wantName:   "bob person",
			otherEmail: alice.email,
		},
	} {
		t.Run(subject.name, func(t *testing.T) {
			resp, me := getMe(t, s, subject.token, "/api/v1/me")

			t.Logf("GET /me as %s -> %d %s", subject.name, resp.status, resp.raw)

			if resp.status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.status, resp.raw)
			}

			if me.UserID != subject.wantUserID {
				t.Errorf("user_id = %q, want %q", me.UserID, subject.wantUserID)
			}

			if me.Email != subject.wantEmail {
				t.Errorf("email = %q, want %q", me.Email, subject.wantEmail)
			}

			if me.DisplayName != subject.wantName {
				t.Errorf("display_name = %q, want %q", me.DisplayName, subject.wantName)
			}

			// The negative, stated separately: a response carrying the other
			// member's address would be a real disclosure even though both of
			// them are in the same organization.
			if strings.Contains(resp.raw, subject.otherEmail) {
				t.Errorf("/me carries the other member's address: %s", resp.raw)
			}

			// Additive: everything the previous shape promised is still there,
			// unchanged. apps/web is being written against this response right
			// now.
			if me.Role == "" || me.SessionID == "" {
				t.Errorf("role or session_id disappeared from /me: %s", resp.raw)
			}

			if me.Organization["id"] != alice.tenantID.String() {
				t.Errorf("organization = %v, want alice's organization %s", me.Organization, alice.tenantID)
			}

			if len(me.Organizations) == 0 {
				t.Errorf("organizations is empty: %s", resp.raw)
			}
		})
	}
}

// TestMeAgreesWithTheMemberListItReplaces checks the fields are the same ones
// the web shell was reading out of GET /members.
//
// Issue #75 is a performance and least-privilege change, not a behaviour
// change: if /me disagreed with /members about the caller's own name, deleting
// the /members call would silently alter what the shell renders.
func TestMeAgreesWithTheMemberListItReplaces(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")

	_, me := getMe(t, s, alice.accessToken, "/api/v1/me")

	members := s.do(t, http.MethodGet, "/api/v1/members", alice.accessToken, nil)
	if members.status != http.StatusOK {
		t.Fatalf("GET /members: %d %s", members.status, members.raw)
	}

	var listed struct {
		Members []struct {
			UserID      string `json:"user_id"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		} `json:"members"`
	}

	decodeInto(t, members, &listed)

	var found bool

	for _, member := range listed.Members {
		if member.UserID != me.UserID {
			continue
		}

		found = true

		t.Logf("/me says %s / %q; /members says %s / %q",
			me.Email, me.DisplayName, member.Email, member.DisplayName)

		if member.Email != me.Email || member.DisplayName != me.DisplayName {
			t.Errorf("/me and /members disagree about the caller: %q/%q vs %q/%q",
				me.Email, me.DisplayName, member.Email, member.DisplayName)
		}
	}

	if !found {
		t.Fatal("the caller is not in their own member list; the fixture is broken")
	}
}

// TestMeCannotBePointedAtAnotherAccount is the disclosure claim, against a real
// database.
//
// /me now returns an email address, so the question "whose" has an answer that
// matters. auth_bola_test.go asks it of a fake that models the policy; this
// asks it of the policy itself, with a second real account whose id and address
// the attacker actually knows.
func TestMeCannotBePointedAtAnotherAccount(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	// Bob joins alice's organization, so his row is *visible* in her tenant.
	// That is the harder case: the policy is not doing the refusing here, the
	// absence of any way to name him is.
	if added := addMember(t, s, alice.accessToken, bob.email, ""); added.status != http.StatusCreated {
		t.Fatalf("adding bob: %d %s", added.status, added.raw)
	}

	t.Logf("alice will try to read bob (user=%s, %s) through /me", bob.userID, bob.email)

	for _, attack := range []struct {
		name    string
		path    string
		headers map[string]string
	}{
		{name: "user_id query parameter", path: "/api/v1/me?user_id=" + bob.userID.String()},
		{name: "sub query parameter", path: "/api/v1/me?sub=" + bob.userID.String()},
		{name: "id query parameter", path: "/api/v1/me?id=" + bob.userID.String()},
		{name: "email query parameter", path: "/api/v1/me?email=" + bob.email},
		{name: "organization_id query parameter", path: "/api/v1/me?organization_id=" + bob.tenantID.String()},
		{name: "X-User-ID header", path: "/api/v1/me", headers: map[string]string{"X-User-Id": bob.userID.String()}},
		{name: "X-User-Email header", path: "/api/v1/me", headers: map[string]string{"X-User-Email": bob.email}},
		{
			name:    "X-Tenant-ID header",
			path:    "/api/v1/me",
			headers: map[string]string{"X-Tenant-ID": bob.tenantID.String()},
		},
		{name: "a user id in the path", path: "/api/v1/me/" + bob.userID.String()},
		{name: "a user id as a sibling route", path: "/api/v1/users/" + bob.userID.String()},
	} {
		t.Run(attack.name, func(t *testing.T) {
			req := s.request(t, http.MethodGet, attack.path, alice.accessToken, nil)

			for name, value := range attack.headers {
				req.Header.Set(name, value)
			}

			resp := s.send(t, req)

			t.Logf("alice, %s -> %d %s", attack.name, resp.status, resp.raw)

			if strings.Contains(resp.raw, bob.email) {
				t.Errorf("BOLA: /me returned bob's address\n%s", resp.raw)
			}

			if strings.Contains(resp.raw, bob.userID.String()) {
				t.Errorf("BOLA: /me returned bob's user id\n%s", resp.raw)
			}

			// A refusal is fine (the two path attacks are 404s), but a 200 has
			// to be alice's own row. "Not bob" alone would also be satisfied by
			// a response steered to some third account.
			if resp.status == http.StatusOK {
				var me meBody

				decodeInto(t, resp, &me)

				if me.UserID != alice.userID.String() || me.Email != alice.email {
					t.Errorf("/me answered with %s / %s, want alice (%s / %s)",
						me.UserID, me.Email, alice.userID, alice.email)
				}
			}
		})
	}
}
