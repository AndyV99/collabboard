/**
 * The human-readable identity behind the session.
 *
 * # Why this needs a request at all
 *
 * The session cookie holds a user id and the active organization, and nothing
 * else — those are the only things `POST /auth/login` returns. `GET /me` adds
 * the role, the session id and the list of organizations, and still no name or
 * address: the API has no endpoint that reports *your own* display name.
 *
 * The one place both exist is `GET /members`, which lists everyone in the
 * token's organization with their name and email. So the shell finds itself in
 * that list. It is a heavier request than the job deserves — it reads every
 * member to render one name — and it is filed upstream as "`GET /me` should
 * return the caller's email and display name", which would delete this file's
 * reason to exist.
 *
 * # Every failure degrades, and none of them redirects
 *
 * A `unauthorized` here is genuine — `serverApi` cannot refresh, and `proxy.ts`
 * already had its chance before the render — so it is tempting to redirect to
 * sign in. That would be a loop. The cookies are still on the browser and
 * nothing in a Server Component can clear them, so the sign-in page would see a
 * session, bounce the visitor back, and the shell would fail the same way again.
 *
 * Instead the shell renders with the organization from the cookie and no name.
 * The next mutation the user attempts goes through a Route Handler, which *can*
 * refresh and *can* clear cookies, and that is where a dead session is turned
 * into a clean sign-out.
 */

import { serverApi } from "@/lib/api/server";
import { listMembers } from "@/lib/api/endpoints";
import { logEvent } from "@/lib/log";

/** Who the signed-in user is, as far as the shell can tell. */
export type Viewer = {
  displayName: string;
  email: string;
};

/**
 * The signed-in user's name and address, or null when they could not be read.
 *
 * Null is a normal outcome, not an error: the API may be down, the session may
 * have been revoked, or the account may simply not appear in its own
 * organization's member list — which is what issue #34's half-registered state
 * looks like from in here.
 */
export async function loadViewer(userId: string): Promise<Viewer | null> {
  const result = await serverApi(listMembers());

  if (!result.ok) {
    logEvent("info", "could not read the signed-in member", {
      event: "web.shell.viewer_unavailable",
      kind: result.error.kind,
    });

    return null;
  }

  const self = result.data.find((member) => member.userId === userId);

  if (self === undefined) {
    return null;
  }

  return { displayName: self.displayName, email: self.email };
}
