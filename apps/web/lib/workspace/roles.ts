/**
 * What the signed-in user's role lets them do, and where that role is read
 * from.
 *
 * # The token's role claim is not the authority, and this file does not use it
 *
 * `session.organization.role` is in the session cookie, and the shell renders it
 * as a badge. It comes from the access token, which is minted at login and
 * re-derived at most once per access-token lifetime — so a member who was
 * promoted a minute ago still carries `member`, and an admin who was demoted
 * still carries `admin`.
 *
 * `apps/api` refuses to trust it for the same reason. ADR 0008 is explicit:
 * `AddMember` reads the caller's row from `memberships` inside the tenant
 * transaction, and does it *again* in the transaction that performs the insert,
 * so "the actor held the role at the instant the row was written" is true rather
 * than "held it a moment earlier".
 *
 * `GET /members` lists exactly that table, for exactly that tenant. So the
 * members screen already holds the authoritative answer, and
 * {@link roleInOrganization} reads it from there — the same value the API will
 * check, one request rather than two, and no chance of the screen offering an
 * action the server is about to refuse because the cookie was stale.
 *
 * # Absence is not permission
 *
 * {@link roleInOrganization} returns null when the caller does not appear in
 * the list, which is what issue #34's half-registered account looks like from in
 * here, and what a removed member's still-valid access token looks like for the
 * rest of its lifetime. Null grants nothing: {@link canAddMembers} is false and
 * {@link grantableRoles} is empty. The screen says why instead of showing a
 * button that would 403.
 */

/** The three roles in `memberships`' CHECK constraint (migration 00002). */
export const ROLE_OWNER = "owner";
export const ROLE_ADMIN = "admin";
export const ROLE_MEMBER = "member";

/** Anything with a `userId` and a `role`. Keeps this module free of `lib/api`. */
type RoleBearing = { userId: string; role: string };

/**
 * The signed-in user's role as the *database* currently has it, or null when
 * they are not in the list.
 *
 * See the module comment for why this is read from `GET /members` rather than
 * from the session cookie.
 */
export function roleInOrganization(
  members: readonly RoleBearing[],
  userId: string,
): string | null {
  return members.find((member) => member.userId === userId)?.role ?? null;
}

/**
 * Whether this role may add someone to the organization.
 *
 * `owner` and `admin`, mirroring `authorizeAddMember`. A `member` may not, and
 * ADR 0008 gives the reason that makes it structural rather than a preference:
 * `member` is the role every added account gets, so a `member` who could add
 * would be enough to grow an organization without limit.
 */
export function canAddMembers(role: string | null): boolean {
  return role === ROLE_OWNER || role === ROLE_ADMIN;
}

/**
 * The roles this role may grant, in the order the form offers them.
 *
 * Mirrors the table in ADR 0008 exactly: an owner may grant `member` or
 * `admin`, an admin may grant `member` only, and nobody may grant `owner`
 * through this endpoint — `validateAddMember` refuses it as a property of the
 * endpoint rather than of the caller, "so no owner discovers that theirs almost
 * could".
 *
 * `member` is first because it is the default the API applies to an omitted
 * role, and because the safe choice should be the one already selected.
 */
export function grantableRoles(role: string | null): readonly string[] {
  switch (role) {
    case ROLE_OWNER:
      return [ROLE_MEMBER, ROLE_ADMIN];
    case ROLE_ADMIN:
      return [ROLE_MEMBER];
    default:
      return [];
  }
}

/** What a role means, for the one-line explanation next to the choice. */
export function describeRole(role: string): string {
  switch (role) {
    case ROLE_OWNER:
      return "Full access, including adding admins.";
    case ROLE_ADMIN:
      return "Can add members.";
    case ROLE_MEMBER:
      return "Can use every project and board here.";
    default:
      return "";
  }
}
