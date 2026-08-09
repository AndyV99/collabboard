/**
 * Posting a form to this app's own auth Route Handlers.
 *
 * The browser talks to `/api/auth/*` on this origin and never to the Go API.
 * That is not a convenience: `app/api/proxy` refuses the `auth` prefix outright,
 * so there is no path from client JavaScript to `POST /auth/login` at all, and
 * therefore no way for a browser to be handed a refresh token by asking. See
 * ADR 0007.
 *
 * `credentials: "same-origin"` is what lets the handler's `Set-Cookie` land.
 * It is the default for same-origin requests, and it is written out anyway
 * because a session that silently stops working when someone changes a base URL
 * is a bad afternoon.
 */

import {
  type AuthFailure,
  networkFailure,
  readErrorMessage,
} from "./outcomes";

/** Route Handler paths. Not the Go API's — see the module comment. */
export const LOGIN_PATH = "/api/auth/login";
export const REGISTER_PATH = "/api/auth/register";

export type SubmitResult = { ok: true } | { ok: false; failure: AuthFailure };

/** Maps a failed response onto copy. One per form; see `lib/auth/outcomes.ts`. */
export type FailureDescriber = (
  status: number,
  apiMessage: string | null,
  headers: Headers,
) => AuthFailure;

/**
 * Posts JSON to an auth Route Handler and reduces the answer to ok-or-copy.
 *
 * The success body is deliberately discarded. It carries the user id and the
 * organization, and both are already in the cookies the handler just set — a
 * screen that reads them from here would be holding session state in a place
 * the server cannot invalidate.
 */
export async function submitAuth(
  path: string,
  body: unknown,
  describe: FailureDescriber,
  fetchImpl: typeof fetch = fetch,
): Promise<SubmitResult> {
  let response: Response;

  try {
    response = await fetchImpl(path, {
      method: "POST",
      credentials: "same-origin",
      headers: { "content-type": "application/json", accept: "application/json" },
      body: JSON.stringify(body),
    });
  } catch {
    // The handler is on this origin, so a rejected fetch is the browser being
    // offline, not the credentials being wrong.
    return { ok: false, failure: networkFailure() };
  }

  if (response.ok) {
    return { ok: true };
  }

  return {
    ok: false,
    failure: describe(response.status, await readErrorMessage(response), response.headers),
  };
}
