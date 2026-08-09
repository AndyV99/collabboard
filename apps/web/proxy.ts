/**
 * Keeps the access token fresh before anything renders.
 *
 * (`proxy.ts` is what Next 16 renamed `middleware.ts` to. It defaults to the
 * Node.js runtime, so `process.env.API_URL` is a real runtime read here and
 * #16's contract holds — an Edge-runtime version would have inlined it at build
 * time, which is the exact failure that issue removed.)
 *
 * # Why this file has to exist
 *
 * A Server Component cannot set a cookie: by the time one renders, the response
 * has begun. So an RSC that meets an expired access token has no way to renew
 * it — it could call `/auth/refresh`, but the rotated refresh token it got back
 * would have nowhere to go, and the cookie would be left holding a token the API
 * now treats as a replay. One render would cost the user their session.
 *
 * This runs before the render and can set cookies, so it is where the renewal
 * happens. By the time a page renders, the access token is either fresh or
 * absent — never expired-but-renewable.
 *
 * # It runs everywhere, but only refreshes on page requests
 *
 * **It does not refresh on `/api/*`.** Those are this app's own Route Handlers,
 * and they refresh for themselves when they get a 401. If this refreshed there
 * too, both would spend the same refresh token — the handler reads the request's
 * cookies, which are the pre-refresh ones — and the second spend is a replay that
 * revokes the session. One refresher per request.
 *
 * **It still runs on `/api/*`, to strip {@link FORWARDED_SESSION_HEADER}.** That
 * header is unauthenticated by construction: it is base64 JSON with no signature,
 * because the only thing that was ever supposed to write it is this file. Leaving
 * `/api/*` out of the matcher meant a client could send its own, and
 * `getServerSession()` would believe it — which turns any leaked 15-minute access
 * token into a 14-day cookie session by way of `POST /api/auth/organization`.
 * Stripping unconditionally, on every path, is what makes "only this file writes
 * that header" true rather than merely intended. Found in review; the test is in
 * `__tests__/auth-routes.test.ts`.
 *
 * **It also stamps the requested path onto the request.** A layout is not told
 * its own URL, and the protected layout needs one to send an unauthenticated
 * visitor to sign in and back again. Same discipline as above — `set`, not
 * `append`, on every path — and the value is only ever read through
 * `safeReturnPath`. See `lib/session/request-path.ts`.
 *
 * **It never redirects.** Signing out is a state, not a navigation: the cookies
 * are cleared and the render sees no session. Where an unauthenticated visitor
 * should be sent is a decision for the screen that knows what it needed, made
 * once, in one place — and a redirect issued from here, on every path including
 * whatever the sign-in page turns out to be, is precisely how that becomes a
 * loop. Because this file has no `NextResponse.redirect` in it, that loop is not
 * reachable rather than merely guarded against.
 */

import { NextResponse } from "next/server";
import type { NextRequest } from "next/server";

import {
  clearSessionCookies,
  metadataFromTokens,
  readSessionCookies,
  writeSessionCookies,
} from "@/lib/session/cookies";
import {
  encodeForwardedSession,
  FORWARDED_SESSION_HEADER,
  stripForwardedSession,
} from "@/lib/session/forward";
import { resolveProxySession } from "@/lib/session/proxy-session";
import { setRequestPath } from "@/lib/session/request-path";

/**
 * Paths that get the header strip and nothing else.
 *
 * `/api/` because those handlers refresh for themselves. `/healthz` because a
 * readiness probe must not be able to put an API round trip in front of itself —
 * see the reasoning in `lib/readiness.ts` about what "ready" means here.
 */
function strippedOnly(pathname: string): boolean {
  return pathname.startsWith("/api/") || pathname === "/healthz";
}

export default async function proxy(request: NextRequest): Promise<NextResponse> {
  // Always start from headers with any client-supplied session header removed,
  // on every path and whatever the outcome. Server code must only ever see one
  // this file wrote.
  const requestHeaders = stripForwardedSession(request.headers);

  // Which page was asked for, so the protected layout can send an
  // unauthenticated visitor to sign in *and back again*. Written with `set` on
  // every path, which is also how a client-supplied copy is discarded. It never
  // becomes a destination without going through `safeReturnPath` — see
  // `lib/session/request-path.ts`.
  setRequestPath(requestHeaders, request.nextUrl);

  if (strippedOnly(request.nextUrl.pathname)) {
    return NextResponse.next({ request: { headers: requestHeaders } });
  }

  const stored = readSessionCookies(request.cookies);
  const now = Date.now();
  const action = await resolveProxySession(stored, now);

  if (action.kind === "unchanged") {
    return NextResponse.next({ request: { headers: requestHeaders } });
  }

  if (action.kind === "signed-out") {
    const response = NextResponse.next({ request: { headers: requestHeaders } });

    clearSessionCookies(response.cookies);

    return response;
  }

  // Refreshed. The browser gets the new cookies on the response; the render
  // needs them *now*, and the request it is given still carries the old ones —
  // hence the header.
  requestHeaders.set(
    FORWARDED_SESSION_HEADER,
    encodeForwardedSession({
      accessToken: action.tokens.accessToken,
      metadata: metadataFromTokens(action.tokens, now),
    }),
  );

  const response = NextResponse.next({ request: { headers: requestHeaders } });

  writeSessionCookies(response.cookies, action.tokens, now);

  return response;
}

export const config = {
  /**
   * Everything except Next's static output.
   *
   * `/api/` is deliberately **in** — `strippedOnly` handles it — because a path
   * this does not run on is a path where a client can supply its own
   * {@link FORWARDED_SESSION_HEADER}. Narrowing this matcher again reopens that
   * hole, which is why `__tests__/auth-routes.test.ts` asserts the pattern
   * matches `/api/...` rather than only asserting the handler's behaviour.
   *
   * `_next/static` and `_next/image` stay out: they are build output, they can
   * carry no session, and running anything in front of a JS chunk would put it
   * on the critical path of every asset on the page.
   */
  matcher: ["/((?!_next/static|_next/image|favicon.ico).*)"],
};
