/**
 * Where the session lives.
 *
 * # The decision
 *
 * `POST /api/v1/auth/login` returns a 15-minute JWT access token *and* a
 * 14-day opaque refresh token in the response body. That shape is right for a
 * separate-origin SPA and wrong for this app, which is same-origin server-side
 * rendered Next.js. This module is the answer: **the browser never receives
 * either token.** A Route Handler on this origin calls the Go API, takes the
 * tokens out of the body, and puts them in `httpOnly` cookies. Client
 * JavaScript cannot read them, so an XSS that can run arbitrary script in this
 * origin still cannot exfiltrate a credential that outlives the page.
 *
 * The alternative — holding tokens in a JS variable — was rejected on two
 * counts, either of which is enough. Tokens in memory are unreachable during
 * server rendering, so every authenticated screen would have to be a Client
 * Component with a loading state, which throws away the reason this app is
 * RSC-first. And they are lost on reload, so a refresh of the page is a
 * re-login unless the refresh token is persisted somewhere the browser can read
 * — which is the thing we are trying to avoid.
 *
 * No change to `apps/api` was needed for this. The Go service keeps returning
 * tokens in the body; this app is simply the only client of that body.
 *
 * # Three cookies, all httpOnly
 *
 * - `cb_at` — the access token. Sent to the Go API as a bearer token by the
 *   server-side transport.
 * - `cb_rt` — the refresh token. Read only by `proxy.ts` and the handlers under
 *   `app/api/auth`. It is never put in a response body, never forwarded to a
 *   render, and never logged.
 * - `cb_session` — user id, active organization, and when the access token
 *   expires. Not a credential, but `httpOnly` anyway: a Server Component reads
 *   it and passes the parts a screen needs down as props, which keeps one rule
 *   ("no session cookie is readable by script") instead of two.
 *
 * The metadata cookie is what lets `proxy.ts` refresh *before* a request fails
 * rather than after, and it saves a `GET /me` round trip on every render.
 *
 * # SameSite and CSRF
 *
 * `sameSite: "lax"` means the browser does not attach these to a cross-site
 * POST, which is the CSRF vector that matters for the mutating Route Handlers.
 * `lib/session/origin.ts` adds a same-origin check on top, because relying on a
 * single control for CSRF is how CSRF happens.
 *
 * `secure` is on everywhere except plain-HTTP localhost, where setting it would
 * mean the cookie is silently dropped and the whole thing appears not to work.
 */

import type { Organization, SessionTokens } from "@/lib/api/types";

/** Access token cookie. */
export const ACCESS_COOKIE = "cb_at";

/** Refresh token cookie. The one this whole module exists to protect. */
export const REFRESH_COOKIE = "cb_rt";

/** Non-credential session metadata. */
export const SESSION_COOKIE = "cb_session";

/** Every cookie this app sets, for clearing them as a set. */
export const SESSION_COOKIE_NAMES = [
  ACCESS_COOKIE,
  REFRESH_COOKIE,
  SESSION_COOKIE,
] as const;

/**
 * How long the cookies live.
 *
 * Mirrors `AUTH_REFRESH_TOKEN_TTL`'s default in `apps/api/internal/config`
 * (14 days). The API does not report the refresh token's lifetime in its
 * response, so this is a mirror of a default rather than a fact — flagged in
 * the PR. Being wrong in either direction is safe: a cookie that outlives the
 * server-side session produces a failed refresh, which signs the user out
 * cleanly, and one that expires early just signs them out early.
 *
 * All three cookies share this max-age. In particular the access token cookie
 * is *not* scoped to `expires_in`: if it vanished at 15 minutes we would have a
 * window where `cb_rt` says "there is a session" and `cb_at` says nothing, and
 * two sources of truth about freshness. `cb_session.accessExpiresAt` is the one
 * source instead.
 */
export const SESSION_MAX_AGE_SECONDS = 14 * 24 * 60 * 60;

/**
 * How early a token counts as expired.
 *
 * A token with eight seconds left will be expired by the time the API sees it,
 * so refreshing at expiry-minus-skew turns a guaranteed 401-and-retry into a
 * pre-emptive refresh. Also absorbs clock drift between this container and the
 * API's.
 */
export const EXPIRY_SKEW_SECONDS = 30;

/** The parsed contents of {@link SESSION_COOKIE}. */
export type SessionMetadata = {
  userId: string;
  organization: Organization;
  /** Unix milliseconds. */
  accessExpiresAt: number;
};

/** What the server knows about the caller's session, from its cookies. */
export type StoredSession = {
  accessToken: string | null;
  refreshToken: string | null;
  metadata: SessionMetadata | null;
};

/** The minimal reader both `next/headers` cookies and `NextRequest.cookies` satisfy. */
export type CookieReader = {
  get(name: string): { value: string } | undefined;
};

/** The minimal writer both `next/headers` cookies and `NextResponse.cookies` satisfy. */
export type CookieWriter = {
  set(name: string, value: string, options: CookieOptions): unknown;
};

/** The subset of cookie attributes this module sets. */
export type CookieOptions = {
  httpOnly: boolean;
  secure: boolean;
  sameSite: "lax" | "strict" | "none";
  path: string;
  maxAge: number;
};

/**
 * Whether to mark cookies `secure`.
 *
 * `NODE_ENV` rather than a bespoke flag: `next dev` is the only situation in
 * which this app is served over plain HTTP, and it is exactly the situation
 * `NODE_ENV !== "production"` describes. A production build served over HTTP is
 * a deployment mistake that this correctly refuses to accommodate.
 */
export function cookiesAreSecure(
  env: Readonly<Record<string, string | undefined>> = process.env,
): boolean {
  return env.NODE_ENV === "production";
}

/** Attributes shared by all three cookies. */
export function sessionCookieOptions(
  env: Readonly<Record<string, string | undefined>> = process.env,
): CookieOptions {
  return {
    httpOnly: true,
    secure: cookiesAreSecure(env),
    sameSite: "lax",
    path: "/",
    maxAge: SESSION_MAX_AGE_SECONDS,
  };
}

/**
 * Encodes session metadata for a cookie value.
 *
 * base64url of JSON: compact, and free of the `;` and `,` that would need
 * quoting in a cookie value. It is *not* a signature and is not treated as one
 * — the metadata is derived from a response this server received and is only
 * ever used for display and for scheduling a refresh. Every authorization
 * decision is made by the Go API against the access token it verifies.
 */
export function encodeMetadata(metadata: SessionMetadata): string {
  return Buffer.from(JSON.stringify(metadata), "utf8").toString("base64url");
}

/** Decodes {@link encodeMetadata}, returning null for anything unexpected. */
export function decodeMetadata(value: string | undefined): SessionMetadata | null {
  if (value === undefined || value === "") {
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

  const { userId, organization, accessExpiresAt } = parsed as Record<string, unknown>;

  if (
    typeof userId !== "string" ||
    typeof accessExpiresAt !== "number" ||
    !Number.isFinite(accessExpiresAt) ||
    typeof organization !== "object" ||
    organization === null
  ) {
    return null;
  }

  const org = organization as Record<string, unknown>;

  if (
    typeof org.id !== "string" ||
    typeof org.name !== "string" ||
    typeof org.slug !== "string" ||
    typeof org.role !== "string"
  ) {
    return null;
  }

  return {
    userId,
    organization: { id: org.id, name: org.name, slug: org.slug, role: org.role },
    accessExpiresAt,
  };
}

/** Turns a login/refresh response into the metadata cookie's contents. */
export function metadataFromTokens(
  tokens: SessionTokens,
  now: number = Date.now(),
): SessionMetadata {
  return {
    userId: tokens.userId,
    organization: tokens.organization,
    accessExpiresAt: now + tokens.expiresIn * 1000,
  };
}

/** Reads whatever session cookies are present. */
export function readSessionCookies(cookies: CookieReader): StoredSession {
  return {
    accessToken: cookies.get(ACCESS_COOKIE)?.value ?? null,
    refreshToken: cookies.get(REFRESH_COOKIE)?.value ?? null,
    metadata: decodeMetadata(cookies.get(SESSION_COOKIE)?.value),
  };
}

/**
 * Writes all three cookies from a fresh token response.
 *
 * Always all three together. A partial write is how you end up with an access
 * token whose metadata describes the previous organization.
 */
export function writeSessionCookies(
  cookies: CookieWriter,
  tokens: SessionTokens,
  now: number = Date.now(),
  env: Readonly<Record<string, string | undefined>> = process.env,
): void {
  const options = sessionCookieOptions(env);

  cookies.set(ACCESS_COOKIE, tokens.accessToken, options);
  cookies.set(REFRESH_COOKIE, tokens.refreshToken, options);
  cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens, now)), options);
}

/**
 * Clears all three cookies.
 *
 * Written as an empty value with `maxAge: 0` rather than `delete`, because
 * `NextResponse.cookies.delete` and `next/headers`' `delete` disagree about
 * attribute matching across path and secure, and a cookie that fails to clear
 * is a user stuck in a signed-out state that keeps presenting a dead token.
 * Setting the same name/path/secure triple to an expired empty value is the one
 * form both honour.
 */
export function clearSessionCookies(
  cookies: CookieWriter,
  env: Readonly<Record<string, string | undefined>> = process.env,
): void {
  const options = { ...sessionCookieOptions(env), maxAge: 0 };

  for (const name of SESSION_COOKIE_NAMES) {
    cookies.set(name, "", options);
  }
}

/**
 * Whether the access token described by this metadata should be replaced.
 *
 * True when there is no metadata at all, which is the "we have a refresh token
 * and nothing else" case after an access cookie was dropped.
 */
export function accessTokenIsStale(
  metadata: SessionMetadata | null,
  now: number = Date.now(),
): boolean {
  if (metadata === null) {
    return true;
  }

  return metadata.accessExpiresAt - EXPIRY_SKEW_SECONDS * 1000 <= now;
}
