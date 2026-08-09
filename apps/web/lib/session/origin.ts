/**
 * The CSRF check on this app's own mutating endpoints.
 *
 * Cookie-based sessions bring CSRF with them. `sameSite: "lax"` on all three
 * session cookies already stops the browser attaching them to a cross-site
 * POST, which covers the attack, and this is the second control on top —
 * because a session whose safety rests on one attribute being right is a
 * session that breaks the day someone needs `sameSite: "none"` for an embed.
 *
 * Two signals, in order:
 *
 * 1. `Sec-Fetch-Site`, which every current browser sends and no page script can
 *    forge. `same-origin` and `none` (a user typing the URL) pass; `same-site`
 *    and `cross-site` do not. `same-site` is refused deliberately: a sibling
 *    subdomain is a different trust boundary, and this app has no reason to
 *    accept writes from one.
 * 2. `Origin` compared to the request's own origin, for anything that did not
 *    send `Sec-Fetch-Site`.
 *
 * A request with neither header is refused. That is a non-browser client — curl
 * without `-H Origin`, or a script — and this app's Route Handlers exist to
 * serve its own browser. Anything else should talk to the Go API directly with
 * a bearer token, where CSRF is not a concept because there are no cookies.
 *
 * `Host`/`X-Forwarded-Host` are not consulted: both are attacker-controllable
 * in the general case. `request.nextUrl.origin` is what Next resolved the
 * request to, which is the same value the browser used to decide whether to
 * send the cookies.
 */

/** Why a request was refused. Ends up in a log line, so it is a closed set. */
export type OriginRejection = "cross_site" | "origin_mismatch" | "no_origin_signal";

export type OriginCheck =
  | { allowed: true }
  | { allowed: false; reason: OriginRejection };

/**
 * Decides whether a state-changing request may proceed.
 *
 * `requestOrigin` is the origin this app was reached on — `nextUrl.origin` in a
 * Route Handler, not the `Host` header.
 */
export function checkSameOrigin(headers: Headers, requestOrigin: string): OriginCheck {
  const fetchSite = headers.get("sec-fetch-site");

  if (fetchSite !== null) {
    return fetchSite === "same-origin" || fetchSite === "none"
      ? { allowed: true }
      : { allowed: false, reason: "cross_site" };
  }

  const origin = headers.get("origin");

  if (origin === null) {
    return { allowed: false, reason: "no_origin_signal" };
  }

  return origin === requestOrigin
    ? { allowed: true }
    : { allowed: false, reason: "origin_mismatch" };
}
