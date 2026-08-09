/**
 * `GET /api/auth/session` — who the browser is signed in as.
 *
 * Client Components cannot read the session cookies, so this is how one finds
 * out whether there is a session and which organization it is in. It answers
 * from the metadata cookie without touching the API, so it costs nothing.
 *
 * It returns no token, and it is the *only* GET on this origin that reports
 * session state, which makes "what can a client learn about the session" a
 * one-file answer.
 *
 * A Server Component should not call this. It should call `getServerSession()`
 * and pass what a child needs down as props — one fewer round trip and no
 * loading state.
 */

import { cookies } from "next/headers";

import { readSessionCookies } from "@/lib/session/cookies";
import { jsonError, jsonOk } from "@/lib/session/route-helpers";

export async function GET(): Promise<Response> {
  const stored = readSessionCookies(await cookies());

  if (stored.metadata === null || stored.refreshToken === null) {
    return jsonError(401, "Not signed in.");
  }

  return jsonOk({
    user_id: stored.metadata.userId,
    organization: stored.metadata.organization,
    // Milliseconds since the epoch, so a client can schedule a pre-emptive
    // refresh instead of waiting for a 401. Not a credential and not a claim
    // the API trusts — it is this app's own note about its own cookie.
    access_expires_at: stored.metadata.accessExpiresAt,
  });
}
