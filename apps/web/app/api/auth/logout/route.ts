/**
 * `POST /api/auth/logout`.
 *
 * Revokes the session on the API and clears the cookies. The cookies are
 * cleared **whatever the API says** — including when it cannot be reached at
 * all. A logout that leaves the browser holding a live cookie because a network
 * call failed is a logout that did not happen, and the user has already been
 * told it did.
 *
 * The API answers 204 for an unknown refresh token rather than 404, so a second
 * logout is not an error.
 */

import type { NextRequest } from "next/server";
import { cookies } from "next/headers";

import { clearSessionCookies, REFRESH_COOKIE } from "@/lib/session/cookies";
import { logout } from "@/lib/session/auth-api";
import { guardOrigin, noContent } from "@/lib/session/route-helpers";
import { logEvent } from "@/lib/log";

export async function POST(request: NextRequest): Promise<Response> {
  const refused = guardOrigin(request);

  if (refused !== null) {
    return refused;
  }

  const jar = await cookies();
  const refreshToken = jar.get(REFRESH_COOKIE)?.value;

  if (refreshToken !== undefined && refreshToken !== "") {
    const result = await logout(refreshToken);

    if (!result.ok) {
      // Worth a line: the server-side session may still be live for up to the
      // refresh token's remaining lifetime, and this is the only record that it
      // was meant to be revoked.
      logEvent("warn", "logout could not revoke the session", {
        event: "web.auth.logout_revoke_failed",
        kind: result.error.kind,
        http_status: result.error.status,
      });
    }
  }

  clearSessionCookies(jar);

  logEvent("info", "session ended", { event: "web.session.ended", reason: "logout" });

  return noContent();
}
