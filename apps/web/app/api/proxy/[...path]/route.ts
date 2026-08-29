/**
 * `/api/proxy/*` — how a Client Component reaches the API.
 *
 * The browser holds no token, so it cannot call the Go API directly. It calls
 * this instead: same-origin, cookies attached by the browser, bearer token
 * attached here from the httpOnly access cookie, and — because this is a Route
 * Handler — a 401 can be recovered from by refreshing and writing new cookies
 * before retrying, which is something a Server Component could not do.
 *
 * What it is *not*: a general reverse proxy. `lib/api/proxy-route.ts` holds an
 * allowlist of resource roots, and `auth` is not on it. There is no path from
 * client JavaScript to `POST /auth/login` or `POST /auth/refresh` on the Go API,
 * so there is no way for a browser to be handed a refresh token.
 *
 * The response is relayed with the API's own status and error envelope, so
 * `lib/api/browser.ts` maps failures exactly as `lib/api/server.ts` does.
 */

import type { NextRequest } from "next/server";
import { cookies } from "next/headers";

import { authenticatedCall } from "@/lib/api/authenticated";
import { relayStatus } from "@/lib/api/errors";
import { type Endpoint, type HttpMethod, apiV1BaseUrl } from "@/lib/api/http";
import {
  PROXIED_METHODS,
  methodHasBody,
  proxyErrorBody,
  proxyTarget,
} from "@/lib/api/proxy-route";
import { clearSessionCookies, writeSessionCookies } from "@/lib/session/cookies";
import { guardOrigin, jsonError } from "@/lib/session/route-helpers";
import { getRefreshToken, getServerSession } from "@/lib/session/server";
import { logEvent } from "@/lib/log";

type Context = { params: Promise<{ path: string[] }> };

/**
 * The relayed answer, wrapped so that nothing about it has to be invented here.
 *
 * Two things need carrying and neither survives an `ApiResult` on its own:
 *
 * - **`empty`**, so a legitimate JSON `null` and a 204 stay distinguishable.
 *   Without it, `parse` returning null would mean "malformed", which is the one
 *   thing a pass-through must not invent.
 * - **`status`**, so the API's own 2xx reaches the browser. `Response.json`
 *   defaults to 200, and every create route on the Go side answers **201** —
 *   `POST /projects`, `/projects/:id/boards`, `/boards/:id/columns`,
 *   `/columns/:id/cards` and `/members` — so without this the module comment
 *   above ("relayed with the API's own status") was true of failures and false
 *   of everything else.
 */
type Relayed = { value: unknown; empty: boolean; status: number };

function passthrough(method: HttpMethod, path: string, body: unknown): Endpoint<Relayed> {
  return {
    method,
    path,
    body,
    // `expectNoContent` only changes behaviour on a 204/205, which among the
    // proxied routes is DELETE. Setting it unconditionally costs nothing and
    // avoids encoding "which methods answer 204" in two places.
    expectNoContent: true,
    parse: (value, status) => ({ value: value ?? null, empty: value === undefined, status }),
  };
}

async function handle(request: NextRequest, context: Context): Promise<Response> {
  const method = request.method as HttpMethod;

  if (!PROXIED_METHODS.has(method)) {
    return jsonError(405, "Method not allowed.");
  }

  // GET is exempt. Not because it "changes nothing" — a GET that 401s does
  // rotate the refresh token on its way to answering — but because there is
  // nothing to gain by forging one: a cross-site subresource GET does not carry
  // the cookies (SameSite=Lax), so it arrives unauthenticated, and the one shape
  // that does carry them, a top-level navigation, cannot read the JSON back.
  // The rotation is idempotent from the user's point of view and leaves them
  // signed in.
  if (method !== "GET") {
    const refused = guardOrigin(request);

    if (refused !== null) {
      return refused;
    }
  }

  const { path: segments } = await context.params;
  const target = proxyTarget(segments, request.nextUrl.search);

  if (!target.allowed) {
    logEvent("warn", "proxy refused a path", {
      event: "web.proxy.refused",
      reason: target.reason,
      // The first segment only, truncated. The full path can carry ids, this
      // line is about which surface was attempted rather than which object, and
      // the value is client-controlled and unbounded.
      root: (segments[0] ?? "").slice(0, 64),
    });

    return jsonError(404, "Not found.");
  }

  let body: unknown;

  if (methodHasBody(method)) {
    const text = await request.text();

    if (text !== "") {
      try {
        body = JSON.parse(text);
      } catch {
        return jsonError(400, "The request body is not valid JSON.");
      }
    }
  }

  const session = await getServerSession();
  const refreshToken = await getRefreshToken();
  const jar = await cookies();

  const result = await authenticatedCall(passthrough(method, target.path, body), {
    baseUrl: apiV1BaseUrl(),
    accessToken: session?.accessToken ?? null,
    refreshToken,
    onRefreshed: (tokens) => writeSessionCookies(jar, tokens),
    onSignedOut: () => clearSessionCookies(jar),
  });

  if (!result.ok) {
    const status = relayStatus(result.error);
    const headers: Record<string, string> = { "cache-control": "no-store" };

    if (result.error.retryAfterSeconds !== undefined) {
      headers["retry-after"] = String(result.error.retryAfterSeconds);
    }

    return Response.json(proxyErrorBody(result.error), { status, headers });
  }

  // `result.data.status` rather than a literal, in both branches. The empty one
  // is reached for a 204 and a 205, and hard-coding 204 there would rewrite a
  // 205 into something with different semantics — the same class of quiet
  // rewrite this issue is about, one status along.
  if (result.data.empty) {
    return new Response(null, {
      status: result.data.status,
      headers: { "cache-control": "no-store" },
    });
  }

  return Response.json(result.data.value, {
    status: result.data.status,
    headers: { "cache-control": "no-store" },
  });
}

export const GET = handle;
export const POST = handle;
export const PATCH = handle;
export const DELETE = handle;
