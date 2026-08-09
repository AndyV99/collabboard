/**
 * `POST /api/auth/organization` — switch the active organization.
 *
 * A Route Handler rather than a proxied call because the API answers with a
 * *whole new session*: a new session id, a new refresh token, and the old
 * session revoked. All three cookies have to be rewritten together, which only
 * something that can set cookies may do.
 *
 * The API re-checks membership against the authenticated subject, so a 403 here
 * is the designed answer for "not a member of that organization" — and for an
 * organization that does not exist, which is deliberately the same answer.
 */

import type { NextRequest } from "next/server";
import { cookies } from "next/headers";

import { switchOrganization } from "@/lib/session/auth-api";
import { writeSessionCookies } from "@/lib/session/cookies";
import { getServerSession } from "@/lib/session/server";
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

  const session = await getServerSession();

  if (session === null) {
    return jsonError(401, "Your session has expired. Sign in again.");
  }

  const body = await readJsonBody(request);
  const organizationId =
    typeof body === "object" && body !== null
      ? (body as Record<string, unknown>).organization_id
      : undefined;

  if (typeof organizationId !== "string" || organizationId === "") {
    return jsonError(400, "organization_id is required.");
  }

  const result = await switchOrganization(session.accessToken, organizationId);

  if (!result.ok) {
    return relayApiError(result.error);
  }

  writeSessionCookies(await cookies(), result.data);

  logEvent("info", "organization switched", {
    event: "web.auth.organization_switched",
    user_id: result.data.userId,
    organization_id: result.data.organization.id,
  });

  return jsonOk({
    user_id: result.data.userId,
    organization: result.data.organization,
  });
}
