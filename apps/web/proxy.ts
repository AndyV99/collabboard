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
 * # Two things it deliberately does not do
 *
 * **It does not run on `/api/*`.** Those are this app's own Route Handlers, and
 * they refresh for themselves when they get a 401. If this ran there too, both
 * would spend the same refresh token — the handler reads the request's cookies,
 * which are the pre-refresh ones — and the second spend is a replay that revokes
 * the session. One refresher per request.
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

export default async function proxy(request: NextRequest): Promise<NextResponse> {
  // Always start from headers with any client-supplied session header removed,
  // on every path and whatever the outcome. A render must only ever see one this
  // file wrote.
  const requestHeaders = stripForwardedSession(request.headers);

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
   * Everything except this app's own API routes and Next's static output.
   *
   * `api` is excluded for the reason in the file comment — one refresher per
   * request. `_next/static` and `_next/image` are build output and images;
   * running a possible API round trip in front of a JS chunk would put the
   * refresh on the critical path of every asset on the page.
   */
  matcher: ["/((?!api/|_next/static|_next/image|favicon.ico).*)"],
};
