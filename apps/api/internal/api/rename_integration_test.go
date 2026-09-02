//go:build integration

package api_test

// Renaming a workspace, end to end, against a real Postgres with the real
// policies (issue #90).
//
// The unit tests in internal/auth own the role ladder and the name bound; the
// BOLA suite owns "whose tenant did the write land in" against a fake. This file
// owns the two claims only a real database can settle:
//
//  1. `UPDATE organizations SET name = @name WHERE id = public.current_tenant_id()`
//     touches one row, in one tenant, and leaves `slug` alone. A query that
//     dropped the predicate would still pass every unit test in this repository,
//     because the fake has one organization per transaction and the policy is
//     what makes that true in production.
//  2. the rename is visible to the *other* members of that workspace and to
//     nobody outside it.

import (
	"net/http"
	"strings"
	"testing"
)

// organizationFromMe reads the caller's current workspace out of GET /me.
func organizationFromMe(t *testing.T, s *server, token string) map[string]any {
	t.Helper()

	resp := s.do(t, http.MethodGet, "/api/v1/me", token, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("GET /me: status %d, body %s", resp.status, resp.raw)
	}

	organization, ok := resp.body["organization"].(map[string]any)
	if !ok {
		t.Fatalf("GET /me has no organization: %s", resp.raw)
	}

	return organization
}

func TestRenamingAWorkspaceLandsInOneTenantAndKeepsTheSlug(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	before := organizationFromMe(t, s, alice.accessToken)
	bobBefore := organizationFromMe(t, s, bob.accessToken)

	t.Logf("alice: %v", before)
	t.Logf("bob:   %v", bobBefore)

	renamed := s.do(t, http.MethodPatch, "/api/v1/organizations", alice.accessToken,
		map[string]string{"name": "Alice And Company"})

	t.Logf("rename -> %d %s", renamed.status, renamed.raw)

	if renamed.status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", renamed.status, renamed.raw)
	}

	after := organizationFromMe(t, s, alice.accessToken)

	if after["name"] != "Alice And Company" {
		t.Errorf("after the rename /me says %v, want %q", after["name"], "Alice And Company")
	}

	// The slug is frozen. Asserted against the database's own view rather than
	// against the rename response, so a handler that echoed the old slug while
	// the UPDATE changed it would still fail.
	if after["slug"] != before["slug"] {
		t.Errorf("slug changed from %v to %v; the rename is not supposed to touch it",
			before["slug"], after["slug"])
	}

	if after["id"] != before["id"] {
		t.Fatalf("the organization id changed from %v to %v", before["id"], after["id"])
	}

	// The row the predicate excludes. Without the WHERE clause -- or without
	// the policy -- this is the assertion that fails.
	bobAfter := organizationFromMe(t, s, bob.accessToken)

	if bobAfter["name"] != bobBefore["name"] {
		t.Fatalf("bob's workspace was renamed to %v by alice's request", bobAfter["name"])
	}
}

// A member sees the rename; a member cannot make one.
func TestWhoMayRenameAWorkspace(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")
	bob := s.register(t, "bob")

	added := addMember(t, s, alice.accessToken, bob.email, "member")
	if added.status != http.StatusCreated {
		t.Fatalf("adding bob: status %d, body %s", added.status, added.raw)
	}

	// Bob acts inside alice's organization from here on.
	switched := s.do(t, http.MethodPost, "/api/v1/auth/organization", bob.accessToken,
		map[string]string{"organization_id": alice.tenantID.String()})
	if switched.status != http.StatusOK {
		t.Fatalf("bob switching into alice's organization: status %d, body %s",
			switched.status, switched.raw)
	}

	bobInAlices := stringField(t, switched.body, "access_token")

	refused := s.do(t, http.MethodPatch, "/api/v1/organizations", bobInAlices,
		map[string]string{"name": "Bob Was Here"})

	t.Logf("bob renaming alice's workspace -> %d %s", refused.status, refused.raw)

	if refused.status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a member may not rename the workspace", refused.status)
	}

	// And nothing was written. A 403 that had already committed would be the
	// worst of both.
	if name := organizationFromMe(t, s, alice.accessToken)["name"]; name == "Bob Was Here" {
		t.Fatal("the refused rename landed anyway")
	}

	// The owner can, and bob sees it.
	renamed := s.do(t, http.MethodPatch, "/api/v1/organizations", alice.accessToken,
		map[string]string{"name": "Alice And Company"})
	if renamed.status != http.StatusOK {
		t.Fatalf("alice renaming her own workspace: status %d, body %s", renamed.status, renamed.raw)
	}

	if name := organizationFromMe(t, s, bobInAlices)["name"]; name != "Alice And Company" {
		t.Errorf("bob still sees %v after the rename", name)
	}
}

// The application check answers, not the constraint.
//
// Migration 00007 bounds `organizations.name` at 200 characters, and a
// constraint violation would surface as a 500. `validateWorkspaceName` runs
// first, so an over-long rename is a 400 with a sentence naming the field —
// which is the whole reason both exist.
func TestAnOverLongRenameIs400AndNot500(t *testing.T) {
	s := newServer(t, generousLimits())

	alice := s.register(t, "alice")

	before := organizationFromMe(t, s, alice.accessToken)

	refused := s.do(t, http.MethodPatch, "/api/v1/organizations", alice.accessToken,
		map[string]string{"name": strings.Repeat("a", 201)})

	t.Logf("201-character rename -> %d %s", refused.status, refused.raw)

	if refused.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; a 500 here means the CHECK answered instead of the code",
			refused.status)
	}

	if name := organizationFromMe(t, s, alice.accessToken)["name"]; name != before["name"] {
		t.Errorf("the refused rename changed the name to %v", name)
	}

	// Exactly at the limit still works, so the bound is not off by one.
	accepted := s.do(t, http.MethodPatch, "/api/v1/organizations", alice.accessToken,
		map[string]string{"name": strings.Repeat("b", 200)})

	if accepted.status != http.StatusOK {
		t.Fatalf("a 200-character rename: status %d, body %s", accepted.status, accepted.raw)
	}
}
