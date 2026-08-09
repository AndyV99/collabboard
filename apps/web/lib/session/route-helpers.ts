/**
 * The bits every session Route Handler repeats.
 *
 * Kept out of the `app/` tree so they can be unit tested directly: a file under
 * `app/` that is not a route is legal but invites someone to add an export Next
 * treats as a route config.
 */

import type { NextRequest } from "next/server";

import { type ApiError, relayStatus } from "@/lib/api/errors";
import { logEvent } from "@/lib/log";
import { checkSameOrigin } from "./origin";

/** The error envelope this app's own routes use — the same shape as the API's. */
export function jsonError(status: number, message: string, extra?: HeadersInit): Response {
  return Response.json(
    { error: message },
    { status, headers: { "cache-control": "no-store", ...headersToObject(extra) } },
  );
}

function headersToObject(init: HeadersInit | undefined): Record<string, string> {
  if (init === undefined) {
    return {};
  }

  const out: Record<string, string> = {};

  new Headers(init).forEach((value, key) => {
    out[key] = value;
  });

  return out;
}

/** A successful JSON body, always uncached. A session response must not be. */
export function jsonOk(body: unknown, status = 200): Response {
  return Response.json(body, {
    status,
    headers: { "cache-control": "no-store" },
  });
}

/** 204, uncached. */
export function noContent(): Response {
  return new Response(null, { status: 204, headers: { "cache-control": "no-store" } });
}

/**
 * Refuses a state-changing request that did not come from this origin.
 *
 * Returns a 403 to send, or null to proceed. See `lib/session/origin.ts` for
 * why this exists on top of `sameSite: "lax"`.
 */
export function guardOrigin(request: NextRequest): Response | null {
  const verdict = checkSameOrigin(request.headers, request.nextUrl.origin);

  if (verdict.allowed) {
    return null;
  }

  logEvent("warn", "cross-origin request refused", {
    event: "web.csrf.refused",
    reason: verdict.reason,
    path: request.nextUrl.pathname,
  });

  return jsonError(403, "This request did not come from this site.");
}

/**
 * Reads a JSON request body.
 *
 * Returns `undefined` for a body that is absent or not JSON, which the callers
 * turn into a 400. The parse error is not echoed: these bodies contain
 * passwords, and a message quoting the offending input is a message quoting a
 * password into whatever collects client-side errors. `apps/api`'s `bindJSON`
 * makes the same call for the same reason.
 */
export async function readJsonBody(request: Request): Promise<unknown | undefined> {
  try {
    return await request.json();
  } catch {
    return undefined;
  }
}

/**
 * Turns an {@link ApiError} from the Go API into a response for our own client.
 *
 * The status is preserved so the browser client's error mapping is identical
 * whether the call went through here or through `/api/proxy` — except where
 * preserving it would be a lie, which {@link relayStatus} handles.
 */
export function relayApiError(error: ApiError): Response {
  const status = relayStatus(error);
  const headers =
    error.retryAfterSeconds === undefined
      ? undefined
      : { "retry-after": String(error.retryAfterSeconds) };

  return jsonError(status, error.message, headers);
}
