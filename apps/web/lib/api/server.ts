/**
 * The API client for server contexts: Server Components, Server Actions, and
 * Route Handlers.
 *
 * Two entry points, and the difference between them is the whole Server/Client
 * boundary in one file:
 *
 * - {@link serverApi} — for **Server Components**. Reads the session, sends the
 *   request, and does not refresh, because a Server Component cannot set a
 *   cookie and a refresh it could not persist would spend the session's rotating
 *   credential and throw the successor away. Freshness is `proxy.ts`'s job, and
 *   it runs before every render. A 401 here therefore means the session is
 *   genuinely over, and the page should render its signed-out state.
 *
 * - {@link mutableServerApi} — for **Route Handlers and Server Actions**, which
 *   can write cookies. Full 401 → single-flight refresh → one retry, with the
 *   rotated tokens persisted.
 *
 * Both are `ApiCall`s, so they take the same {@link Endpoint} values from
 * `lib/api/endpoints.ts` and a caller reads identically either side of the line.
 */

import { cookies } from "next/headers";

import { apiBaseUrl } from "@/lib/api";
import { clearSessionCookies, writeSessionCookies } from "@/lib/session/cookies";
import { getRefreshToken, getServerSession } from "@/lib/session/server";
import { authenticatedCall } from "./authenticated";
import type { ApiResult } from "./errors";
import { API_V1_PREFIX, type ApiCall, type Endpoint } from "./http";

/** Base URL of the API's v1 surface, resolved from `API_URL` at call time. */
export function apiV1BaseUrl(): string {
  return `${apiBaseUrl()}${API_V1_PREFIX}`;
}

/**
 * Calls the API as the current user, without refreshing.
 *
 * The read-only client. See the module comment for why it does not refresh.
 */
export const serverApi: ApiCall = async <T,>(
  endpoint: Endpoint<T>,
): Promise<ApiResult<T>> => {
  const session = await getServerSession();

  return authenticatedCall(endpoint, {
    baseUrl: apiV1BaseUrl(),
    accessToken: session?.accessToken ?? null,
    // No refresh token and no `onRefreshed`: two independent reasons this
    // cannot refresh, so adding one back by accident is not enough to break the
    // rule above.
    refreshToken: null,
  });
};

/**
 * Calls the API as the current user, refreshing and retrying once on a 401.
 *
 * Only usable where `cookies().set()` is legal — a Route Handler or a Server
 * Action. Calling it during Server Component rendering throws from Next, which
 * is the correct failure: it says "use `serverApi` here" rather than silently
 * losing a rotated refresh token.
 */
export const mutableServerApi: ApiCall = async <T,>(
  endpoint: Endpoint<T>,
): Promise<ApiResult<T>> => {
  const session = await getServerSession();
  const refreshToken = await getRefreshToken();
  const jar = await cookies();

  return authenticatedCall(endpoint, {
    baseUrl: apiV1BaseUrl(),
    accessToken: session?.accessToken ?? null,
    refreshToken,
    onRefreshed: (tokens) => writeSessionCookies(jar, tokens),
    onSignedOut: () => clearSessionCookies(jar),
  });
};
