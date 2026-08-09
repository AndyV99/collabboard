/**
 * `POST /api/auth/login` — the one place tokens turn into cookies.
 *
 * The browser posts credentials here; this handler posts them to the Go API,
 * takes the access and refresh tokens out of the response body, writes them
 * into httpOnly cookies, and returns **only** the non-secret part of the
 * session. Nothing a client can read ever contains a token.
 *
 * That is the whole trick, and it needed no change to `apps/api`.
 */

import type { NextRequest } from "next/server";
import { cookies } from "next/headers";

import { writeSessionCookies } from "@/lib/session/cookies";
import { login } from "@/lib/session/auth-api";
import {
  guardOrigin,
  jsonError,
  jsonOk,
  readJsonBody,
  relayApiError,
} from "@/lib/session/route-helpers";
import { logEvent } from "@/lib/log";

export async function POST(request: NextRequest): Promise<Response> {
  const refused = guardOrigin(request);

  if (refused !== null) {
    return refused;
  }

  const body = await readJsonBody(request);

  if (typeof body !== "object" || body === null) {
    return jsonError(400, "Email and password are required.");
  }

  const { email, password } = body as Record<string, unknown>;

  // Validated here rather than relayed, so an empty form does not cost a round
  // trip and, more importantly, does not consume a slot in the API's
  // per-address login budget.
  if (typeof email !== "string" || typeof password !== "string") {
    return jsonError(400, "Email and password are required.");
  }

  const result = await login({ email, password });

  if (!result.ok) {
    // No log line naming the address. A failed-login log keyed by email is an
    // enumeration oracle for anyone who can read logs, and the API already logs
    // the attempt on the side that can do something about it.
    logEvent("info", "login rejected", {
      event: "web.auth.login_failed",
      kind: result.error.kind,
    });

    return relayApiError(result.error);
  }

  writeSessionCookies(await cookies(), result.data);

  logEvent("info", "session established", {
    event: "web.auth.login",
    user_id: result.data.userId,
    organization_id: result.data.organization.id,
  });

  return jsonOk({
    user_id: result.data.userId,
    organization: result.data.organization,
  });
}
