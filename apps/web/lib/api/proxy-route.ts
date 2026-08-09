/**
 * The policy half of `app/api/proxy/[...path]/route.ts`, kept here so it can be
 * tested as a function rather than as a route.
 *
 * # Why an allowlist and not a denylist
 *
 * This handler forwards a browser-supplied path to the Go API with a bearer
 * token attached from an httpOnly cookie. That is exactly the shape of a
 * confused deputy: whatever a client can name here, it can reach as the signed-in
 * user. A denylist would work today and would silently start forwarding the next
 * route someone adds to the API.
 *
 * The list below is the set of resource roots the UI needs. It excludes:
 *
 * - **`auth`** — every auth endpoint either returns or consumes a refresh
 *   token. `POST /api/proxy/auth/login` would hand the browser a refresh token
 *   in a response body and undo the entire cookie design. This is the single
 *   most important entry not on the list.
 * - **`ws`** — a WebSocket upgrade cannot be proxied through a Route Handler
 *   anyway, and issue #9's client connects with a token passed to it as a prop.
 */

import type { ApiError } from "./errors";
import type { HttpMethod } from "./http";

/** Resource roots a browser may reach through the proxy. */
export const PROXIED_ROOTS: ReadonlySet<string> = new Set([
  "me",
  "members",
  "projects",
  "boards",
  "columns",
  "cards",
]);

/** Methods the proxy forwards. Anything else is a 405. */
export const PROXIED_METHODS: ReadonlySet<string> = new Set([
  "GET",
  "POST",
  "PATCH",
  "DELETE",
]);

export type ProxyTarget =
  | { allowed: true; path: string }
  | { allowed: false; reason: "empty_path" | "not_allowed" };

/**
 * Turns the catch-all segments into a path on the API's v1 surface.
 *
 * Segments arrive already percent-decoded from Next, so they are re-encoded on
 * the way out: a segment containing `/` or `?` must not be able to change the
 * shape of the URL that gets built.
 */
export function proxyTarget(segments: readonly string[], search = ""): ProxyTarget {
  if (segments.length === 0 || segments[0] === "") {
    return { allowed: false, reason: "empty_path" };
  }

  if (!PROXIED_ROOTS.has(segments[0])) {
    return { allowed: false, reason: "not_allowed" };
  }

  const path = segments.map((segment) => encodeURIComponent(segment)).join("/");

  return { allowed: true, path: `/${path}${search}` };
}

/**
 * The response body shape the proxy sends back for a failure.
 *
 * Identical to the API's own `{"error": "..."}`, so the browser client's error
 * mapping does not need to know whether a call was proxied.
 */
export function proxyErrorBody(error: ApiError): { error: string } {
  return { error: error.message };
}

/** Whether this method may carry a request body. */
export function methodHasBody(method: HttpMethod): boolean {
  return method === "POST" || method === "PATCH";
}
