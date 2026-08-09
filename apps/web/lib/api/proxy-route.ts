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
 *
 * # Why dot segments are refused rather than escaped
 *
 * `encodeURIComponent("..")` is `".."` — `.` is unreserved, so percent-encoding
 * does not neutralise it — and the URL parser inside `fetch` removes dot
 * segments when it resolves the string. So an allowlisted first segment followed
 * by `..` walks straight back out of it:
 *
 *     ["cards", "..", "auth", "organization"]
 *       → "/cards/../auth/organization"
 *       → http://api/api/v1/auth/organization
 *
 * which is `POST /auth/organization` — an endpoint that answers with a refresh
 * token in its body. The allowlist would have "passed" and the browser would
 * have been handed the one credential this whole design exists to keep from it.
 *
 * Hence two independent guards below: no segment may be `.`, `..` or empty, and
 * the assembled path must still begin with the root that was allowed. The second
 * is the one that survives someone later deciding the first is too strict.
 *
 * # The allowlist checks the first segment only
 *
 * `/projects/<id>/anything` is forwarded even though `lib/api/endpoints.ts`
 * never builds such a path — the API answers 404 for routes it does not have,
 * and enumerating full path shapes here would mean maintaining the route table
 * twice. What the list constrains is which *surface* a browser can reach, and
 * with dot segments refused, the first segment is the whole surface.
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
  | { allowed: false; reason: "empty_path" | "not_allowed" | "dot_segment" };

/**
 * Segments that must never appear, whatever they are wrapped in.
 *
 * Compared after decoding, so `%2E%2E` and `..` are the same thing here — which
 * is the point, since Next hands these over already decoded.
 */
const DOT_SEGMENTS = new Set([".", ".."]);

/**
 * Turns the catch-all segments into a path on the API's v1 surface.
 *
 * Segments arrive already percent-decoded from Next, so they are re-encoded on
 * the way out: a segment containing `/` or `?` must not be able to change the
 * shape of the URL that gets built. Dot segments are refused rather than encoded
 * — see the module comment for why encoding them does not work.
 */
export function proxyTarget(segments: readonly string[], search = ""): ProxyTarget {
  if (segments.length === 0 || segments[0] === "") {
    return { allowed: false, reason: "empty_path" };
  }

  const root = segments[0];

  if (!PROXIED_ROOTS.has(root)) {
    return { allowed: false, reason: "not_allowed" };
  }

  if (segments.some((segment) => segment === "" || DOT_SEGMENTS.has(segment))) {
    return { allowed: false, reason: "dot_segment" };
  }

  const path = `/${segments.map((segment) => encodeURIComponent(segment)).join("/")}`;

  // The second guard, independent of the first. Whatever the segments turned
  // out to be, the path we are about to send must still address the surface
  // that was allowed. If this ever fails, the check above has a hole in it.
  if (path !== `/${root}` && !path.startsWith(`/${root}/`)) {
    return { allowed: false, reason: "dot_segment" };
  }

  return { allowed: true, path: `${path}${search}` };
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
