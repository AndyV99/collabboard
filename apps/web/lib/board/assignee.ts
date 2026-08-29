/**
 * Turning a card's `assignee_id` into something a person recognises.
 *
 * A card carries a `user_id` and nothing else — `cardBody` has no name in it,
 * deliberately, because a card is not the place to publish the directory. The
 * name comes from `GET /members`, which the board page already reads and which
 * is scoped to the caller's organization by the token's own org claim.
 *
 * Both functions are pure and take the member list as an argument, so the
 * lookup is a unit test rather than a rendered board.
 */

import type { Member } from "@/lib/api/types";

/**
 * The display name of the member a card is assigned to, or null.
 *
 * Null covers two different situations and does not distinguish them, because
 * the screen cannot act differently on them:
 *
 * - **the member list did not load** (`members` is null) — the board is still
 *   correct about *that* a card is assigned, just not about to whom;
 * - **the id is not in the list** — a card assigned by somebody else to a
 *   member added since this page was read. `assignee_id` references
 *   `memberships` with `ON DELETE SET NULL`, so a revoked member's cards are
 *   unassigned by the database rather than left pointing at a stranger; a
 *   missing name is therefore a stale list, not a dangling reference.
 *
 * The caller renders "assigned, name unknown" for both rather than inventing a
 * name or, worse, printing the raw uuid — which would be a user id on screen in
 * exchange for telling nobody anything.
 */
export function assigneeName(
  members: readonly Member[] | null,
  userId: string,
): string | null {
  if (members === null) {
    return null;
  }

  return members.find((member) => member.userId === userId)?.displayName ?? null;
}

/**
 * One or two letters standing in for a name, for the avatar on a card tile.
 *
 * `Array.from` rather than indexing, because `"👩‍🔬 Ada"[0]` is half a surrogate
 * pair and renders as a replacement character. Display names are free text and
 * the API bounds them by *code points*, so the first unit of a string is not
 * reliably a character — the same counting mistake `maxLengthFor` exists to
 * avoid, one layer up.
 *
 * The avatar is decorative: every place it appears is accompanied by the full
 * name in the accessible name, so two ambiguous letters cost a sighted reader a
 * glance and cost a screen reader user nothing.
 */
export function initialsFor(name: string): string {
  const words = name.trim().split(/\s+/).filter((word) => word !== "");

  if (words.length === 0) {
    return "?";
  }

  const first = Array.from(words[0])[0] ?? "";
  const last = words.length === 1 ? "" : (Array.from(words[words.length - 1])[0] ?? "");

  return `${first}${last}`.toLocaleUpperCase();
}
