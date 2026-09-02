/**
 * Who the signed-in user is, for the shell.
 *
 * # What this replaced, and why the old shape was worth deleting
 *
 * Until #75 the API had no endpoint that reported *your own* name: the session
 * cookie holds a user id and the active organization, and `GET /me` reported the
 * role, the session id and the organizations but no identity. The one place a
 * name and an address existed together was `GET /members`, so the shell listed
 * every colleague in the organization and found itself in the result.
 *
 * That was O(members) work on every protected page render, and it read every
 * colleague's email address in order to display one. It also coupled the
 * signed-in frame to the membership surface, which it has no other reason to
 * call — a `GET /members` in the API's request log for a page that shows no
 * members is a question nobody could answer from the URL.
 *
 * `meResponse` now carries `email` and `display_name`, so this is one request
 * for exactly the row it needs and there is nothing to search.
 *
 * # Every failure degrades, and none of them redirects
 *
 * An `unauthorized` here is genuine — `serverApi` cannot refresh, and `proxy.ts`
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

import { currentUser } from "@/lib/api/endpoints";
import { serverApi } from "@/lib/api/server";
import type { CurrentUser } from "@/lib/api/types";
import { logEvent } from "@/lib/log";

/**
 * The signed-in principal, or null when it could not be read.
 *
 * Null is a normal outcome rather than an error: the API may be down, the
 * session may have been revoked between the cookie being written and this
 * render, or the body may not be the shape this client understands.
 *
 * It takes no user id, and that absence is the whole of #78. The old version
 * needed one because it was searching a list for you; this one asks the
 * question directly, so there is no id to pass and no way to ask it about
 * somebody else.
 */
export async function loadCurrentUser(): Promise<CurrentUser | null> {
  const result = await serverApi(currentUser());

  if (!result.ok) {
    logEvent("info", "could not read the signed-in principal", {
      event: "web.shell.viewer_unavailable",
      kind: result.error.kind,
    });

    return null;
  }

  return result.data;
}
