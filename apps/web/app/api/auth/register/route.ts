/**
 * `POST /api/auth/register`.
 *
 * Registration does not start a session: `POST /api/v1/auth/register` returns
 * the new user and its first organization, no tokens. This handler relays that
 * faithfully rather than logging the user in behind their back — the sign-up
 * screen calls `/api/auth/login` next, which is one extra round trip and keeps
 * "a session exists" the result of presenting a password.
 *
 * A duplicate address is a 409, which the API chooses deliberately (see the
 * comment on `registerHandler`): it discloses that an address is registered,
 * and the alternative needs a mailer this service does not have.
 */

import type { NextRequest } from "next/server";

import { register } from "@/lib/session/auth-api";
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
    return jsonError(400, "Email, password and display name are required.");
  }

  const { email, password, display_name: displayName, organization_name: orgName } =
    body as Record<string, unknown>;

  if (
    typeof email !== "string" ||
    typeof password !== "string" ||
    typeof displayName !== "string"
  ) {
    return jsonError(400, "Email, password and display name are required.");
  }

  if (orgName !== undefined && typeof orgName !== "string") {
    return jsonError(400, "Organization name must be text.");
  }

  const result = await register({
    email,
    password,
    displayName,
    organizationName: orgName,
  });

  if (!result.ok) {
    logEvent("info", "registration rejected", {
      event: "web.auth.register_failed",
      kind: result.error.kind,
    });

    return relayApiError(result.error);
  }

  logEvent("info", "account registered", {
    event: "web.auth.register",
    user_id: result.data.userId,
    organization_id: result.data.organization.id,
  });

  return jsonOk(
    {
      user_id: result.data.userId,
      email: result.data.email,
      display_name: result.data.displayName,
      organization: result.data.organization,
    },
    201,
  );
}
