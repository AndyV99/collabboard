/**
 * The one place an unauthenticated visitor is turned around.
 *
 * # Why it is here and not in `proxy.ts`
 *
 * ADR 0007 is explicit that nothing in the session layer redirects: `proxy.ts`
 * has no `NextResponse.redirect` in it, so the "redirect everything that has no
 * session" loop — which catches the sign-in page too, because the sign-in page
 * also has no session — is not reachable rather than merely guarded against.
 *
 * The decision is a screen's to make, and this is where the screens make it. It
 * is called from the protected layout, so it is one call for a whole subtree
 * rather than one per page, and adding a page under that layout cannot forget
 * it.
 *
 * # `redirect()` throws
 *
 * `next/navigation`'s `redirect` works by throwing a control-flow signal Next
 * catches. So this function's `Promise<ServerSession>` return type is honest —
 * it either returns a session or does not return — but it also means a
 * `try/catch` around a call to it would swallow the redirect. Do not wrap it.
 */

import { redirect } from "next/navigation";

import { signInHref } from "@/lib/auth/routes";
import { currentRequestPath } from "./request-path";
import { type ServerSession, getRenderSession } from "./server";

/**
 * The current session, or a redirect to sign in that returns here afterwards.
 *
 * Uses the render reader, so a session `proxy.ts` refreshed a moment ago counts
 * — the request's own cookies are the pre-refresh ones and would look expired.
 */
export async function requireSession(): Promise<ServerSession> {
  const session = await getRenderSession();

  if (session !== null) {
    return session;
  }

  redirect(signInHref(await currentRequestPath()));
}
