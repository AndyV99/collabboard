/**
 * `GET /api/realtime/boards/:boardId` — the browser's window onto the board's
 * event stream.
 *
 * # Why this route exists at all
 *
 * The Go API authenticates a WebSocket handshake with a bearer token carried in
 * a `Sec-WebSocket-Protocol` offer, because a browser cannot set headers on a
 * handshake. ADR 0007 decided the browser never receives a bearer token,
 * because a token in a JS variable is a token an XSS can exfiltrate. Those two
 * facts are in direct conflict, and #66 is where the conflict has to be
 * resolved.
 *
 * It is resolved here: **the handshake happens on this server, where the token
 * already is.** The browser opens a same-origin `EventSource`-shaped stream
 * authenticated by the httpOnly cookies it already has, and this handler holds
 * the WebSocket. No credential crosses into JavaScript, so ADR 0007 is
 * untouched rather than traded away. `docs/adr/0010-realtime-browser-credential.md`
 * records the options and what this costs.
 *
 * # Why it is a Route Handler and not a custom server
 *
 * `next.config.ts` sets `output: "standalone"`, and a custom `server.js` that
 * proxied the upgrade would replace the server that build produces. A Route
 * Handler returning a `ReadableStream` needs none of that and deploys as the
 * same image.
 *
 * # What this handler does not do
 *
 * It does not reconnect, and it does not refresh the token. Both belong to the
 * browser: a reconnect must be followed by a re-fetch of the board (ADR 0005),
 * and only the browser can ask for the Server Component render that performs
 * one. A refresh has to write cookies, which a handler that has already begun
 * streaming its response cannot do — so a `4001` is relayed, and the browser
 * calls `POST /api/auth/refresh` and opens a new stream, which reads the cookie
 * that call just wrote.
 */

import type { NextRequest } from "next/server";

import { apiBaseUrl } from "@/lib/api";
import { logEvent } from "@/lib/log";
import { relayBoardStream } from "@/lib/realtime/relay";
import { apiWebSocketUrl, looksLikeUuid } from "@/lib/realtime/stream";
import { jsonError } from "@/lib/session/route-helpers";
import { getServerSession } from "@/lib/session/server";

type Context = { params: Promise<{ boardId: string }> };

/**
 * Node, and never static.
 *
 * `WebSocket` here is Node's, and the response is a stream that must not be
 * collected, cached or revalidated. `force-dynamic` is what stops the build
 * from treating a GET with no search params as something it could pre-render.
 */
export const runtime = "nodejs";
export const dynamic = "force-dynamic";
export const fetchCache = "force-no-store";

export async function GET(request: NextRequest, context: Context): Promise<Response> {
  const { boardId } = await context.params;

  if (!looksLikeUuid(boardId)) {
    return jsonError(404, "Not found.");
  }

  // Cookies only. This is a Route Handler, so a client can reach it directly,
  // and `getRenderSession()` would consult the unsigned header `proxy.ts` uses
  // to hand a fresh token to a render — which a client can also send. Same rule
  // as `/api/proxy`, same reason.
  const session = await getServerSession();

  if (session === null) {
    return jsonError(401, "Your session has expired. Sign in again.");
  }

  // No origin guard. This is a GET that changes nothing, and `SameSite=Lax`
  // means a cross-site subresource request arrives without the session cookies
  // and is answered with the 401 above. A top-level navigation does carry them
  // but cannot read the body back. The same reasoning `/api/proxy` records for
  // its GET exemption applies, with less at stake: there is no refresh here to
  // be spent.

  let url: string;

  try {
    url = apiWebSocketUrl(apiBaseUrl());
  } catch (error) {
    // `apiBaseUrl` throws only for a misconfigured deployment, which is worth
    // saying out loud rather than reporting to the user as a flaky board.
    logEvent("error", "realtime stream could not resolve the API url", {
      event: "web.realtime.misconfigured",
      reason: error instanceof Error ? error.message : "unknown",
    });

    return jsonError(500, "Live updates are unavailable.");
  }

  logEvent("info", "realtime stream opened", {
    event: "web.realtime.opened",
    board_id: boardId,
    organization_id: session.organization.id,
  });

  const body = relayBoardStream({
    boardId,
    accessToken: session.accessToken,
    url,
    signal: request.signal,
  });

  return new Response(body, {
    headers: {
      "content-type": "text/event-stream; charset=utf-8",
      // `no-transform` matters as much as `no-store`: a proxy that gzips this
      // is a proxy that buffers it, and a buffered event stream is a stream
      // that arrives in one piece when it is already too late to be live.
      "cache-control": "no-store, no-transform",
      connection: "keep-alive",
      // nginx-family proxies buffer proxied responses by default. This is the
      // header that turns it off, and it is inert everywhere else.
      "x-accel-buffering": "no",
    },
  });
}
