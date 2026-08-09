/**
 * How `proxy.ts` tells the render what it just did.
 *
 * A Server Component cannot set a cookie — HTTP has already started streaming
 * by the time one renders — so the refresh has to happen before the render, in
 * `proxy.ts`. But the cookies `proxy.ts` writes land on the *response*: the
 * request being rendered still carries the old, expired ones. Without this
 * header a render immediately after a refresh would use the token that was just
 * replaced and get a 401 it cannot recover from.
 *
 * So the proxy passes the fresh access token forward on the request, and
 * `lib/session/server.ts` prefers it over the cookie.
 *
 * # The refresh token is not in here
 *
 * Deliberately. Rendering code has no reason to hold it, and the narrower the
 * set of places it exists the shorter the list of places it can leak from. The
 * only readers of `cb_rt` in this app are `proxy.ts` and the Route Handlers
 * under `app/api/auth`.
 *
 * # Inbound values are always discarded
 *
 * A client can send any header it likes. {@link stripForwardedSession} runs
 * before the proxy sets its own, so a browser-supplied value can never reach a
 * render. This is not the main line of defence — the Go API verifies the JWT's
 * signature, so an injected token would have to be one the sender already held
 * — but a request header that a render trusts should be one only this app can
 * write.
 */

import type { SessionMetadata } from "./cookies";

/** Request header carrying the post-refresh session to the render. */
export const FORWARDED_SESSION_HEADER = "x-collabboard-session";

/** The payload {@link FORWARDED_SESSION_HEADER} carries. */
export type ForwardedSession = {
  accessToken: string;
  metadata: SessionMetadata;
};

/** Encodes a forwarded session as a header value. */
export function encodeForwardedSession(session: ForwardedSession): string {
  return Buffer.from(JSON.stringify(session), "utf8").toString("base64url");
}

/** Decodes {@link encodeForwardedSession}, returning null for anything else. */
export function decodeForwardedSession(
  value: string | null | undefined,
): ForwardedSession | null {
  if (value === null || value === undefined || value === "") {
    return null;
  }

  let parsed: unknown;

  try {
    parsed = JSON.parse(Buffer.from(value, "base64url").toString("utf8"));
  } catch {
    return null;
  }

  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return null;
  }

  const { accessToken, metadata } = parsed as Record<string, unknown>;

  if (typeof accessToken !== "string" || accessToken === "") {
    return null;
  }

  if (typeof metadata !== "object" || metadata === null) {
    return null;
  }

  const meta = metadata as Record<string, unknown>;
  const organization = meta.organization as Record<string, unknown> | undefined;

  if (
    typeof meta.userId !== "string" ||
    typeof meta.accessExpiresAt !== "number" ||
    organization === undefined ||
    typeof organization.id !== "string" ||
    typeof organization.name !== "string" ||
    typeof organization.slug !== "string" ||
    typeof organization.role !== "string"
  ) {
    return null;
  }

  return {
    accessToken,
    metadata: {
      userId: meta.userId,
      accessExpiresAt: meta.accessExpiresAt,
      organization: {
        id: organization.id,
        name: organization.name,
        slug: organization.slug,
        role: organization.role,
      },
    },
  };
}

/**
 * Removes any client-supplied value, returning a mutable copy.
 *
 * Always call this before setting the header, and call it even when there is
 * nothing to set — "the proxy did not refresh" must not be spoofable into "here
 * is a session".
 */
export function stripForwardedSession(headers: Headers): Headers {
  const copy = new Headers(headers);

  copy.delete(FORWARDED_SESSION_HEADER);

  return copy;
}
