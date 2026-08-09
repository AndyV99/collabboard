/**
 * What the proxy should do about the session on this request.
 *
 * Split out of `proxy.ts` so the decision is a pure-ish function with one async
 * dependency, testable without a server. `proxy.ts` is then only the wiring:
 * read cookies, apply this, write cookies.
 */

import type { SessionTokens } from "@/lib/api/types";
import { type StoredSession, accessTokenIsStale } from "./cookies";
import { type RefreshDeps, refreshSession } from "./refresh";

/**
 * - `unchanged` — leave every cookie alone and render as-is.
 * - `refreshed` — write the new cookies and forward the new access token.
 * - `signed-out` — clear the cookies. The render sees no session.
 */
export type ProxyAction =
  | { kind: "unchanged" }
  | { kind: "refreshed"; tokens: SessionTokens }
  | { kind: "signed-out"; reason: string };

/**
 * Decides, and refreshes if that is the decision.
 *
 * The two cases worth stating explicitly:
 *
 * **A refresh cookie with no usable access token still gets a render.** That is
 * the ordinary "came back after lunch" path: the access token expired, the
 * refresh token has thirteen days left, and refreshing here is what makes the
 * first click after lunch work instead of bouncing to a sign-in screen.
 *
 * **An unreachable API is not a sign-out.** `unavailable` leaves the cookies
 * in place. The page will render whatever its own failure state is, and the
 * next request tries again. Clearing cookies on a transient failure would turn
 * a thirty-second API blip into every signed-in user having to log in again.
 */
export async function resolveProxySession(
  stored: StoredSession,
  now: number = Date.now(),
  deps: RefreshDeps = {},
): Promise<ProxyAction> {
  if (stored.refreshToken === null || stored.refreshToken === "") {
    // A leftover access or metadata cookie with no refresh token cannot be
    // renewed and will produce 401s forever. Clear it once.
    return stored.accessToken === null && stored.metadata === null
      ? { kind: "unchanged" }
      : { kind: "signed-out", reason: "no_refresh_token" };
  }

  if (
    stored.accessToken !== null &&
    stored.accessToken !== "" &&
    !accessTokenIsStale(stored.metadata, now)
  ) {
    return { kind: "unchanged" };
  }

  // `now` is forwarded rather than left to default, so a test that injects an
  // artificial clock for the staleness check above does not silently fall back
  // to wall-clock time for the grace window below.
  const outcome = await refreshSession(stored.refreshToken, deps, now);

  switch (outcome.status) {
    case "refreshed":
      return { kind: "refreshed", tokens: outcome.tokens };
    case "rejected":
      return { kind: "signed-out", reason: outcome.reason };
    default:
      return { kind: "unchanged" };
  }
}
