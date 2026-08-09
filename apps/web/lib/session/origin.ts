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
 * # What `requestOrigin` is, and is not
 *
 * Callers pass `request.nextUrl.origin`, and it is worth being exact about what
 * that is: Next derives it from the request line plus `Host`/`X-Forwarded-Host`.
 * It is **not** an independently trusted value. So the `Origin` branch compares
 * a client-supplied header against a client-supplied host, and on its own it
 * would stop nobody who could set both.
 *
 * That branch is a compatibility fallback, not the control. The control is
 * `Sec-Fetch-Site`, which every current browser sends and no page script can
 * forge, plus `SameSite=Lax` on the cookies themselves — and an attacker who can
 * set arbitrary request headers is not running in a browser and has no cookies
 * to ride on in the first place.
 *
 * Two consequences worth knowing before changing this. Behind a TLS-terminating
 * proxy that does not set `X-Forwarded-Proto`, `nextUrl.origin` is `http://…`
 * while the browser sends `Origin: https://…`, and the fallback would refuse a
 * legitimate request; today `Sec-Fetch-Site` answers first, so it never runs.
 * And if a stronger guarantee is ever wanted, the fix is a configured canonical
 * origin, not a different header.
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
