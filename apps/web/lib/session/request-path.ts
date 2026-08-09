/**
 * How a Server Component finds out which page was asked for.
 *
 * # Why this is not just `usePathname`
 *
 * A protected route has to redirect an unauthenticated visitor to the sign-in
 * screen *and remember where they were going*, and the natural place for that
 * check is one layout rather than every page — a per-page check is a check
 * somebody forgets to add. But a layout is not told its own URL: it renders for
 * a whole subtree, and `usePathname` is a client hook that runs long after the
 * redirect needed to happen.
 *
 * `proxy.ts` does know, it already rewrites the request's headers, and it
 * already runs before every render. So it writes the path onto the request and
 * {@link currentRequestPath} reads it back.
 *
 * # It is not trusted, and does not need to be
 *
 * {@link setRequestPath} uses `set`, not `append`, so a client-supplied copy is
 * overwritten on every path — the same discipline `stripForwardedSession`
 * applies to the session header, for the same reason.
 *
 * That is belt to the braces, though. The value's only use is to become the
 * `next` parameter on a sign-in link, and `safeReturnPath` refuses anything that
 * is not a same-origin absolute path. A forged value can therefore change which
 * page *the forger* is sent to after signing in, and nothing else. Reading it
 * through `safeReturnPath` here rather than at the call site is what makes that
 * true of every caller.
 */

import { headers } from "next/headers";

import { DEFAULT_SIGNED_IN_PATH, safeReturnPath } from "@/lib/auth/routes";

/** Request header carrying the requested path and query to the render. */
export const REQUEST_PATH_HEADER = "x-collabboard-path";

/**
 * Writes the requested path onto a copy of the request's headers.
 *
 * `set` replaces, which is also how any inbound value is discarded. Called on
 * every request `proxy.ts` sees, including the ones it does nothing else for.
 */
export function setRequestPath(target: Headers, url: URL): void {
  target.set(REQUEST_PATH_HEADER, `${url.pathname}${url.search}`);
}

/**
 * The path and query of the page being rendered, as a safe return destination.
 *
 * Falls back to {@link DEFAULT_SIGNED_IN_PATH} when the header is missing — a
 * request that did not go through `proxy.ts`, which in practice means a unit
 * test. Never throws, because "we do not know where you were going" is a worse
 * reason to fail a render than it is to land somebody on their workspace.
 */
export async function currentRequestPath(): Promise<string> {
  const value = (await headers()).get(REQUEST_PATH_HEADER);

  return value === null ? DEFAULT_SIGNED_IN_PATH : safeReturnPath(value);
}
