/**
 * `POST /api/auth/refresh` — the browser's only way to renew a session.
 *
 * It takes no body. The refresh token comes from the httpOnly cookie, which is
 * the point: the caller cannot supply one, cannot read the one being used, and
 * cannot read the one that replaces it.
 *
 * Three answers, and they are three because two of them look the same and must
 * not be treated the same:
 *
 * - **204** — refreshed, new cookies set.
 * - **401** — the session is over. Cookies cleared *before* answering, so the
 *   next request from this browser carries nothing and does not come back here.
 *   That is what stops a failed refresh becoming a loop: the second attempt has
 *   no cookie to attempt with.
 * - **502** — the API could not be reached. Cookies left alone, because a blip
 *   must not sign everyone out; the caller may try again later.
 */

import type { NextRequest } from "next/server";
import { cookies } from "next/headers";

import {
  clearSessionCookies,
  REFRESH_COOKIE,
  writeSessionCookies,
} from "@/lib/session/cookies";
import { refreshSession } from "@/lib/session/refresh";
import { guardOrigin, jsonError, noContent } from "@/lib/session/route-helpers";
import { logEvent } from "@/lib/log";

export async function POST(request: NextRequest): Promise<Response> {
  const refused = guardOrigin(request);

  if (refused !== null) {
    return refused;
  }

  const jar = await cookies();
  const refreshToken = jar.get(REFRESH_COOKIE)?.value ?? null;

  if (refreshToken === null || refreshToken === "") {
    // Nothing to refresh. Clear anything left over — an access cookie without a
    // refresh cookie is a half-state, and leaving it would keep producing 401s
    // from the API that this endpoint could never fix.
    clearSessionCookies(jar);

    return jsonError(401, "Your session has expired. Sign in again.");
  }

  const outcome = await refreshSession(refreshToken);

  if (outcome.status === "refreshed") {
    writeSessionCookies(jar, outcome.tokens);

    return noContent();
  }

  if (outcome.status === "rejected") {
    clearSessionCookies(jar);

    logEvent("info", "session ended", {
      event: "web.session.ended",
      reason: outcome.reason,
    });

    return jsonError(401, "Your session has expired. Sign in again.");
  }

  return jsonError(502, "Could not reach the server. Try again shortly.");
}
