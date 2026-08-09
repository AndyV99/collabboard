/**
 * Reading the session from a server context.
 *
 * Importing `next/headers` makes this module unusable from a Client Component —
 * Next fails the build rather than shipping it to the browser — which is the
 * boundary this whole design rests on, enforced by the compiler rather than by
 * a comment. See `apps/web/README.md` for the rule every later screen follows.
 */

import { cookies, headers } from "next/headers";

import type { Organization } from "@/lib/api/types";
import {
  REFRESH_COOKIE,
  type SessionMetadata,
  readSessionCookies,
} from "./cookies";
import { FORWARDED_SESSION_HEADER, decodeForwardedSession } from "./forward";

/**
 * A session as server code sees it.
 *
 * There is no refresh token on this type on purpose. Rendering code has no use
 * for one, and a field that exists is a field that gets passed somewhere.
 */
export type ServerSession = {
  accessToken: string;
  userId: string;
  organization: Organization;
  /** Unix milliseconds. Present so a caller can reason about staleness. */
  accessExpiresAt: number;
};

/**
 * The current session as recorded in the cookies, or null when there is none.
 *
 * **This is the one to use in a Route Handler.** It reads cookies and nothing
 * else, so no request header a client can set participates in the answer.
 *
 * Returns null rather than throwing when there is no session: "signed out" is a
 * state a page renders, not an exception.
 */
export async function getServerSession(): Promise<ServerSession | null> {
  const stored = readSessionCookies(await cookies());

  if (stored.accessToken === null || stored.metadata === null) {
    return null;
  }

  return sessionFrom(stored.accessToken, stored.metadata);
}

/**
 * The current session for a *render*, preferring what `proxy.ts` just minted.
 *
 * On a request where the proxy refreshed, the request's cookies are the old
 * ones — the new ones are on a response the browser has not received yet — so a
 * render has to be told out of band. That is the only reason
 * {@link FORWARDED_SESSION_HEADER} exists.
 *
 * # Why this is a second function rather than a branch in the first
 *
 * The header is unsigned: `decodeForwardedSession` checks shape, not provenance.
 * `proxy.ts` strips any inbound copy on every path, which is what makes trusting
 * it sound — but that is one control, and it was briefly not true (the matcher
 * excluded `/api/`, so a client could send its own header to a Route Handler and
 * be believed; via `POST /api/auth/organization` that upgraded a 15-minute
 * access token into a 14-day cookie session). Splitting the two readers means a
 * Route Handler does not depend on the strip being right, and the strip does not
 * depend on every handler remembering to. Found in review; both controls are now
 * asserted by tests.
 */
export async function getRenderSession(): Promise<ServerSession | null> {
  const forwarded = decodeForwardedSession(
    (await headers()).get(FORWARDED_SESSION_HEADER),
  );

  if (forwarded !== null) {
    return sessionFrom(forwarded.accessToken, forwarded.metadata);
  }

  return getServerSession();
}

function sessionFrom(accessToken: string, metadata: SessionMetadata): ServerSession {
  return {
    accessToken,
    userId: metadata.userId,
    organization: metadata.organization,
    accessExpiresAt: metadata.accessExpiresAt,
  };
}

/**
 * The refresh token, for the two callers that are allowed one: the Route
 * Handlers under `app/api/auth`, and `app/api/proxy` when it has to refresh
 * before retrying.
 *
 * It reads the cookie directly rather than going through {@link getServerSession}
 * so that "who can get a refresh token" is a `grep` for this function's name.
 */
export async function getRefreshToken(): Promise<string | null> {
  return (await cookies()).get(REFRESH_COOKIE)?.value ?? null;
}
