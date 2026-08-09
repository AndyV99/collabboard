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
 * The current session, or null when there is none.
 *
 * Prefers the header `proxy.ts` forwards over the cookies, because on a request
 * where the proxy just refreshed, the request's cookies are the *old* ones —
 * the new ones are on the response the browser has not received yet.
 *
 * Returns null rather than throwing when there is no session: "signed out" is a
 * state a page renders, not an exception.
 */
export async function getServerSession(): Promise<ServerSession | null> {
  const forwarded = decodeForwardedSession(
    (await headers()).get(FORWARDED_SESSION_HEADER),
  );

  if (forwarded !== null) {
    return sessionFrom(forwarded.accessToken, forwarded.metadata);
  }

  const stored = readSessionCookies(await cookies());

  if (stored.accessToken === null || stored.metadata === null) {
    return null;
  }

  return sessionFrom(stored.accessToken, stored.metadata);
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
