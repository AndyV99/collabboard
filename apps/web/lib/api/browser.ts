/**
 * The API client for Client Components.
 *
 * It never sees a token. Requests go to this app's own origin — `/api/proxy` —
 * where a Route Handler attaches the bearer token from the httpOnly cookie and
 * forwards to the Go API. The browser's only credential is the cookie it cannot
 * read.
 *
 * Consequences worth knowing before using this:
 *
 * - There is no `API_URL` in the browser. The base is a relative path, so #16's
 *   runtime-configuration contract holds for free: nothing about the API's
 *   location is in the client bundle.
 * - `/api/proxy` refuses the `auth` prefix, so this client cannot reach
 *   `POST /auth/login` or `/auth/refresh` on the Go API and therefore cannot
 *   obtain a refresh token by asking for one.
 * - CORS does not exist here: every request is same-origin.
 *
 * # Refreshing
 *
 * On a 401, one `POST /api/auth/refresh` runs and the original request is
 * retried once. Concurrent 401s share that one refresh: {@link refreshOnce}
 * holds a module-level promise, so ten failing requests produce one refresh, not
 * ten. This is the browser half of the same rule `lib/session/refresh.ts`
 * enforces on the server, and it matters for the same reason — the API rotates
 * refresh tokens and treats a replay as grounds to revoke the session.
 */

import type { ApiResult } from "./errors";
import { type ApiCall, type Endpoint, sendRequest } from "./http";

/**
 * Path prefix of the authenticated pass-through on this origin.
 *
 * `/api/proxy/projects` reaches `${API_URL}/api/v1/projects`. Endpoint paths are
 * written without a base precisely so the same value works here and on the
 * server.
 */
export const BROWSER_API_BASE = "/api/proxy";

/** Route Handler that refreshes the session cookies. */
export const REFRESH_ENDPOINT = "/api/auth/refresh";

/** Route Handler that ends the session. */
export const LOGOUT_ENDPOINT = "/api/auth/logout";

/**
 * The single in-flight refresh.
 *
 * Module-level and browser-only, so its scope is one document. A second tab
 * refreshes independently — and lands on the server-side single-flight in
 * `lib/session/refresh.ts`, which de-duplicates across tabs.
 */
let pendingRefresh: Promise<boolean> | null = null;

/** Subscribers notified when the session ends. */
const signedOutListeners = new Set<() => void>();

/**
 * Registers a callback for "the session is over".
 *
 * This layer never navigates. Sign-out is a state — the cookies are gone and
 * every call returns `unauthorized` — and *where* to send the user is a
 * decision for the screen, made once, in one place. A redirect issued from deep
 * inside a fetch helper is how sign-in pages end up redirecting themselves.
 *
 * Returns an unsubscribe function.
 */
export function onSignedOut(listener: () => void): () => void {
  signedOutListeners.add(listener);

  return () => {
    signedOutListeners.delete(listener);
  };
}

function announceSignedOut(): void {
  for (const listener of signedOutListeners) {
    listener();
  }
}

/** Test seam. Production callers pass nothing. */
export type BrowserApiOptions = {
  fetchImpl?: typeof fetch;
  signal?: AbortSignal;
};

/**
 * Refreshes the session, joining a refresh already in progress.
 *
 * Resolves true when the session is usable afterwards. On a definitive failure
 * it notifies {@link onSignedOut} subscribers exactly once — the listeners fire
 * from the single shared attempt, not once per waiting caller.
 */
export function refreshOnce(options: BrowserApiOptions = {}): Promise<boolean> {
  if (pendingRefresh !== null) {
    return pendingRefresh;
  }

  const fetchImpl = options.fetchImpl ?? fetch;

  const attempt = (async () => {
    let response: Response;

    try {
      response = await fetchImpl(REFRESH_ENDPOINT, {
        method: "POST",
        credentials: "same-origin",
        headers: { accept: "application/json" },
      });
    } catch {
      // The refresh endpoint is on this origin, so this is the browser being
      // offline rather than the session being invalid. Not a sign-out.
      return false;
    }

    if (response.ok) {
      return true;
    }

    // The handler answers 401 only when it has already cleared the cookies.
    // Anything else (500, 502 from a proxy) leaves the session alone.
    if (response.status === 401) {
      announceSignedOut();
    }

    return false;
  })().finally(() => {
    pendingRefresh = null;
  });

  pendingRefresh = attempt;

  return attempt;
}

/**
 * Runs an {@link Endpoint} from the browser, refreshing and retrying once on a
 * 401.
 */
export function browserApi(options: BrowserApiOptions = {}): ApiCall {
  return async <T,>(endpoint: Endpoint<T>): Promise<ApiResult<T>> => {
    const send = () =>
      sendRequest(endpoint, {
        baseUrl: BROWSER_API_BASE,
        credentials: "same-origin",
        fetchImpl: options.fetchImpl,
        signal: options.signal,
      });

    const first = await send();

    if (first.ok || first.error.kind !== "unauthorized") {
      return first;
    }

    const refreshed = await refreshOnce(options);

    if (!refreshed) {
      return first;
    }

    // Exactly one retry. A 401 here is final and is returned as one.
    return send();
  };
}

/**
 * Ends the session from the browser.
 *
 * Always reports success to subscribers: whether or not the API acknowledged
 * the revocation, the handler clears the cookies, so this browser is signed out
 * either way and pretending otherwise would strand the user on a screen they no
 * longer have a token for.
 */
export async function signOut(options: BrowserApiOptions = {}): Promise<void> {
  const fetchImpl = options.fetchImpl ?? fetch;

  // Detach any refresh already in flight. Its cookies are about to be cleared,
  // so whatever it resolves to is about a session that no longer exists — and a
  // caller joining it afterwards would be told the session is live.
  pendingRefresh = null;

  try {
    await fetchImpl(LOGOUT_ENDPOINT, {
      method: "POST",
      credentials: "same-origin",
      headers: { accept: "application/json" },
    });
  } catch {
    // Deliberately swallowed; see the doc comment.
  }

  announceSignedOut();
}

/** Test-only: drops any in-flight refresh and every listener. */
export function __resetBrowserApiForTests(): void {
  pendingRefresh = null;
  signedOutListeners.clear();
}

/** Whether a refresh is currently in flight. Used by tests to assert sharing. */
export function refreshInFlight(): boolean {
  return pendingRefresh !== null;
}

/** A default client for callers that need no test seams. */
export const api: ApiCall = browserApi();
