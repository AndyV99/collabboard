/**
 * Who may add a member, and where that answer is read from.
 *
 * The table below is ADR 0008's, and it is asserted here rather than trusted,
 * because the UI's promise — "you are never offered an action that will 403" —
 * is only as good as its agreement with `authorizeAddMember` and
 * `validateAddMember`.
 */

import { describe, expect, it } from "vitest";

import {
  ROLE_ADMIN,
  ROLE_MEMBER,
  ROLE_OWNER,
  canAddMembers,
  grantableRoles,
  roleInOrganization,
} from "@/lib/workspace/roles";

const MEMBERS = [
  { userId: "u-owner", role: ROLE_OWNER },
  { userId: "u-admin", role: ROLE_ADMIN },
  { userId: "u-member", role: ROLE_MEMBER },
];

describe("roleInOrganization", () => {
  it("finds the caller's own row", () => {
    expect(roleInOrganization(MEMBERS, "u-admin")).toBe(ROLE_ADMIN);
  });

  it("is null when the caller is not in the list", () => {
    // What #34's half-registered account looks like from here, and what a
    // removed member's still-valid access token looks like until it expires.
    expect(roleInOrganization(MEMBERS, "u-stranger")).toBeNull();
    expect(roleInOrganization([], "u-owner")).toBeNull();
  });
});

describe("canAddMembers", () => {
  it.each([
    [ROLE_OWNER, true],
    [ROLE_ADMIN, true],
    [ROLE_MEMBER, false],
  ])("%s → %s", (role, allowed) => {
    expect(canAddMembers(role)).toBe(allowed);
  });

  it("refuses an unknown role and a missing one", () => {
    // Absence is not permission. A role the client does not recognise is not
    // one it should extrapolate privileges from either.
    expect(canAddMembers(null)).toBe(false);
    expect(canAddMembers("")).toBe(false);
    expect(canAddMembers("superuser")).toBe(false);
  });
});

describe("grantableRoles", () => {
  it("lets an owner grant member or admin", () => {
    expect(grantableRoles(ROLE_OWNER)).toEqual([ROLE_MEMBER, ROLE_ADMIN]);
  });

  it("lets an admin grant member only", () => {
    // An admin granting an admin is ErrInsufficientRole: it is what stops an
    // addition being a way to manufacture peers.
    expect(grantableRoles(ROLE_ADMIN)).toEqual([ROLE_MEMBER]);
  });

  it("offers a member nothing", () => {
    expect(grantableRoles(ROLE_MEMBER)).toEqual([]);
    expect(grantableRoles(null)).toEqual([]);
  });

  it("never offers owner, whoever is asking", () => {
    // Refused by validateAddMember as a property of the endpoint rather than of
    // the caller, so no owner discovers that theirs almost could.
    for (const role of [ROLE_OWNER, ROLE_ADMIN, ROLE_MEMBER, null]) {
      expect(grantableRoles(role)).not.toContain(ROLE_OWNER);
    }
  });

  it("puts the default first, so the safe choice is preselected", () => {
    expect(grantableRoles(ROLE_OWNER)[0]).toBe(ROLE_MEMBER);
  });
});
