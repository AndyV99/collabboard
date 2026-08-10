/**
 * The contract between this app's own two halves of the realtime path.
 *
 * The browser does not speak to the Go API's WebSocket. It speaks to
 * `GET /api/realtime/boards/:boardId`, a Route Handler on this origin that
 * holds the WebSocket on the browser's behalf and relays what comes down it as
 * Server-Sent Events. See `docs/adr/0010-realtime-browser-credential.md` for
 * why, in one line: the handshake needs a bearer token in a subprotocol offer,
 * and ADR 0007 says the browser never gets a bearer token.
 *
 * This file is the wire format of that relay. It is pure and imports nothing
 * from `next/*`, so both ends can use it and both ends can be tested without
 * one another.
 *
 * # Why an envelope rather than relaying the frames verbatim
 *
 * Two of the three things the browser needs to know are not frames. "The
 * upstream socket is open" and "the upstream socket closed with code 4003" have
 * no representation in the server's own protocol — a close code is not a
 * message — and the whole point of this issue is that the close codes mean
 * different things. Flattening them into one discriminated union means the
 * browser client has exactly one thing to parse.
 */

/** One line of the event stream, once decoded. */
export type StreamMessage =
  /** The upstream WebSocket is open and the subscribe has been sent. */
  | { t: "open" }
  /** A frame from the Go API, verbatim, still to be parsed by `protocol.ts`. */
  | { t: "frame"; frame: unknown }
  /**
   * The upstream WebSocket closed.
   *
   * `code` is null when the relay never got a socket at all — the API was
   * unreachable, or refused the upgrade. That is a different situation from any
   * close code and the recovery treats it as a generic, escalating failure.
   */
  | { t: "closed"; code: number | null; reason: string };

/** Encodes one message as an SSE `data:` line, terminated. */
export function encodeStreamMessage(message: StreamMessage): string {
  return `data: ${JSON.stringify(message)}\n\n`;
}

/**
 * A comment line, sent periodically so the stream is not mistaken for a stalled
 * response.
 *
 * SSE comments are `:`-prefixed and ignored by any reader. They exist here for
 * the hop between the browser and *this* app — a proxy or load balancer with an
 * idle-read timeout will close a connection that has been silent for a minute,
 * and a board where nobody is doing anything is silent by definition. The Go
 * API's own 25-second ping keeps the other hop alive and has nothing to do with
 * this one.
 */
export const STREAM_HEARTBEAT = ": heartbeat\n\n";

/** How often {@link STREAM_HEARTBEAT} is written. Under the usual 60s idle. */
export const STREAM_HEARTBEAT_MS = 20_000;

/** The path a browser opens to watch one board. */
export function boardStreamPath(boardId: string): string {
  return `/api/realtime/boards/${encodeURIComponent(boardId)}`;
}

/**
 * The Go API's WebSocket URL, derived from the HTTP base.
 *
 * `http:` → `ws:` and `https:` → `wss:`, which is the whole transformation.
 * Deriving it rather than adding a second environment variable is deliberate:
 * two variables naming the same service is two things to get out of step, and
 * the failure when they disagree is a realtime path that is silently pointed at
 * the wrong environment while every REST call is correct.
 */
export function apiWebSocketUrl(apiBase: string): string {
  const url = new URL(apiBase);

  url.protocol = url.protocol === "https:" ? "wss:" : "ws:";

  return `${url.href.replace(/\/+$/, "")}/api/v1/ws`;
}

/**
 * Whether a string is shaped like the uuid the API will accept as a board id.
 *
 * Checked before a socket is opened, not for safety — the API validates it, and
 * `parseBoardID` refuses the nil uuid too — but because opening a WebSocket to
 * another service in order to be told the path parameter was nonsense is an
 * expensive way to answer a question this side already knows the answer to.
 */
export function looksLikeUuid(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}
