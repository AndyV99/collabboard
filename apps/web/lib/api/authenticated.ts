/**
 * The 401 → refresh → retry-once rule, with no framework in it.
 *
 * This is the behaviour issue #59 is really about, so it is deliberately in a
 * module that imports neither `next/headers` nor `next/server`: everything it
 * needs arrives as a callback, which is why the tests can drive every path —
 * including the ones that are hard to provoke against a real API — without a
 * server.
 *
 * # The rule
 *
 * 1. Send the request with whatever access token there is.
 * 2. If the answer is anything but 401, that is the answer. A 403 is not
 *    retried: the token was fine and the answer will not change.
 * 3. On 401, refresh **once**, through {@link refreshSession}'s single-flight
 *    so that N concurrent 401s produce one call to `POST /auth/refresh` rather
 *    than N — which on this API would be N-1 replays and a revoked session.
 * 4. Retry the original request **once**. If that is a 401 too, it is returned
 *    as-is. There is no second refresh and no loop.
 *
 * # Who can refresh
 *
 * Only a caller that can persist the result. The API rotates refresh tokens, so
 * a refresh whose new token is thrown away has spent the session's credential
 * for one request and left the cookie holding a token that is now a replay.
 * `onRefreshed` being absent is how a caller says "I cannot store this", and
 * such a caller gets the 401 instead. In practice that is Server Component
 * rendering, where `cookies().set()` is not available — and `proxy.ts` refreshes
 * before the render precisely so that path is not needed.
 */

import { refreshSession } from "@/lib/session/refresh";
import type { SessionTokens } from "./types";
import { type ApiResult, err, errorFromStatus } from "./errors";
import { type Endpoint, sendRequest } from "./http";

/** Everything {@link authenticatedCall} needs, as data and callbacks. */
export type AuthenticatedCallOptions = {
  /** Absolute base including the `/api/v1` prefix. */
  baseUrl: string;
  /** The access token to try first, or null when there is none. */
  accessToken: string | null;
  /** The refresh token, or null when there is no session to refresh. */
  refreshToken: string | null;
  /**
   * Persists a successful refresh. Its absence means this caller cannot store
   * cookies, and disables refresh entirely — see the module comment.
   */
  onRefreshed?: (tokens: SessionTokens) => void;
  /**
   * Called when the API says the session is over, so the caller can clear its
   * cookies. Not called when the refresh merely failed to reach the API: a
   * blip must not sign everyone out.
   */
  onSignedOut?: () => void;
  fetchImpl?: typeof fetch;
  signal?: AbortSignal;
};

/** The 401 this returns when there is no session at all, without a round trip. */
function unauthorized<T>(): ApiResult<T> {
  return err(errorFromStatus(401, undefined));
}

/**
 * Runs one endpoint, refreshing and retrying at most once on a 401.
 */
export async function authenticatedCall<T>(
  endpoint: Endpoint<T>,
  options: AuthenticatedCallOptions,
): Promise<ApiResult<T>> {
  const canRefresh = options.onRefreshed !== undefined && options.refreshToken !== null;

  // No access token but a refresh token we are allowed to spend: refresh first
  // rather than sending a request we know will 401. This is the ordinary path
  // for a browser whose access cookie expired between page loads.
  let accessToken = options.accessToken;

  if ((accessToken === null || accessToken === "") && canRefresh) {
    const refreshed = await tryRefresh(options);

    if (refreshed === null) {
      return unauthorized();
    }

    accessToken = refreshed;
  }

  if (accessToken === null || accessToken === "") {
    return unauthorized();
  }

  const first = await sendRequest(endpoint, {
    baseUrl: options.baseUrl,
    accessToken,
    fetchImpl: options.fetchImpl,
    signal: options.signal,
  });

  if (first.ok || first.error.kind !== "unauthorized" || !canRefresh) {
    return first;
  }

  const rotated = await tryRefresh(options);

  if (rotated === null) {
    // Either the session is over (cookies already cleared by `onSignedOut`) or
    // the refresh endpoint was unreachable. Both leave the caller with the
    // original 401, which is the honest answer: this request did not succeed
    // and we could not make it succeed.
    return first;
  }

  // Exactly one retry. Whatever this returns is the final answer, 401 included.
  return sendRequest(endpoint, {
    baseUrl: options.baseUrl,
    accessToken: rotated,
    fetchImpl: options.fetchImpl,
    signal: options.signal,
  });
}

/**
 * Refreshes and reports the new access token, or null if there is none to be
 * had. Runs `onRefreshed`/`onSignedOut` as the outcome dictates.
 */
async function tryRefresh(options: AuthenticatedCallOptions): Promise<string | null> {
  if (options.refreshToken === null || options.onRefreshed === undefined) {
    return null;
  }

  const outcome = await refreshSession(options.refreshToken, {
    fetchImpl: options.fetchImpl,
    baseUrl: options.baseUrl,
  });

  if (outcome.status === "refreshed") {
    options.onRefreshed(outcome.tokens);

    return outcome.tokens.accessToken;
  }

  if (outcome.status === "rejected") {
    options.onSignedOut?.();
  }

  return null;
}
