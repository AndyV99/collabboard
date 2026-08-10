/**
 * `POST /api/auth/first-organization` — create the workspace a stranded account
 * never got.
 *
 * # Why this is a Route Handler at all
 *
 * The same reason `/api/auth/login` is one: the body carries a password, and the
 * browser must not talk to the Go API directly. It is *not* here because the
 * response needs cookies — it deliberately sets none. `POST /organizations`
 * answers 201 with the organization and no tokens, because a subject with zero
 * memberships cannot hold one (ADR 0009). The client's next call is an ordinary
 * `/api/auth/login`, which is where a session gets minted, and keeping that true
 * is worth the extra round trip: "a session exists" stays the result of
 * presenting a password to the one endpoint that issues one. `/api/auth/register`
 * makes the same choice for the same reason.
 *
 * # Why the path is not `/api/auth/organization`
 *
 * That one is taken, by the *authenticated* organization switch, which returns a
 * whole new session. Two operations whose only similarity is the noun. The name
 * here mirrors `Service.CreateFirstOrganization` in `apps/api`, which is the
 * thing it actually calls.
 */

import type { NextRequest } from "next/server";

import { createFirstOrganization } from "@/lib/session/auth-api";
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

  const { email, password, organization_name: orgName } = body as Record<string, unknown>;

  // Checked here rather than relayed, for the reason `/api/auth/login` gives: a
  // request the API would reject out of hand still spends a slot in the login
  // budget, and this route is charged against that budget *before* the
  // credential is looked at.
  if (typeof email !== "string" || typeof password !== "string") {
    return jsonError(400, "Email and password are required.");
  }

  if (orgName !== undefined && typeof orgName !== "string") {
    return jsonError(400, "Workspace name must be text.");
  }

  const result = await createFirstOrganization({
    email,
    password,
    organizationName: orgName,
  });

  if (!result.ok) {
    // Same rule as the login handler: no log line naming the address. A failure
    // here is a failed credential presentation, and a log keyed by email is an
    // enumeration oracle for anyone who can read logs. The API logs the attempt
    // on the side that can act on it, with `operation: create_organization`.
    logEvent("info", "workspace recovery rejected", {
      event: "web.auth.first_organization_failed",
      kind: result.error.kind,
    });

    return relayApiError(result.error);
  }

  logEvent("info", "workspace created for an account that had none", {
    event: "web.auth.first_organization",
    user_id: result.data.userId,
    organization_id: result.data.organization.id,
  });

  return jsonOk(
    { user_id: result.data.userId, organization: result.data.organization },
    201,
  );
}
