/**
 * The server half of the realtime path: one WebSocket in, one event stream out.
 *
 * This runs on the Next server, which is the only place in the web app that
 * holds an access token. It opens the Go API's WebSocket using the
 * `bearer.collabboard.v1.` subprotocol — the mechanism that exists because a
 * *browser* cannot set a handshake header, used here from a place that could
 * have set one, because using it costs nothing and inventing a second
 * authentication path would have meant changing `apps/api` — and writes what
 * comes back down an SSE body.
 *
 * The socket factory is an argument, so every behaviour below is testable with
 * a fake socket and no network: the subscribe on open, the relay of each frame,
 * the close-code passthrough, the heartbeat, and the teardown when the browser
 * goes away.
 *
 * # This relay is deliberately dumb
 *
 * It does not parse frames, does not decide what a close code means, does not
 * retry, and holds no board state. All of that is the browser client's job, in
 * `use-board-live.ts` and `recovery.ts`. The reason is that a reconnect must
 * re-subscribe *and re-fetch the board*, and the re-fetch is a React Server
 * Component render that only the browser can ask for. A relay that quietly
 * reconnected underneath would hide the one event — a fresh `subscribed` — that
 * ADR 0005 requires the client to react to, and the board would go stale in
 * exactly the situation the ADR is about.
 */

import { BEARER_SUBPROTOCOL_PREFIX, SUBPROTOCOL } from "./protocol";
import {
  STREAM_HEARTBEAT,
  STREAM_HEARTBEAT_MS,
  type StreamMessage,
  encodeStreamMessage,
} from "./stream";

/** The part of `WebSocket` this relay uses. Kept small so a fake is small. */
export type RelaySocket = {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  addEventListener(type: "open", handler: () => void): void;
  addEventListener(type: "message", handler: (event: { data: unknown }) => void): void;
  addEventListener(
    type: "close",
    handler: (event: { code: number; reason: string }) => void,
  ): void;
  addEventListener(type: "error", handler: () => void): void;
};

export type RelayOptions = {
  boardId: string;
  accessToken: string;
  /** The API's WebSocket URL, from `apiWebSocketUrl`. */
  url: string;
  /** Aborted when the browser disconnects. */
  signal: AbortSignal;
  /** Injectable for tests. Defaults to the global `WebSocket` (Node 22+). */
  openSocket?: (url: string, protocols: string[]) => RelaySocket;
  heartbeatMs?: number;
};

/**
 * Opens the upstream socket and returns the SSE body for the browser.
 *
 * The stream ends when the upstream socket closes, and never on its own. A
 * relay that ended the response early would look to the browser exactly like a
 * dropped connection, which it would then reconnect and re-fetch for — correct,
 * but a re-fetch nobody needed.
 */
export function relayBoardStream(options: RelayOptions): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  const openSocket = options.openSocket ?? defaultOpenSocket;
  const heartbeatMs = options.heartbeatMs ?? STREAM_HEARTBEAT_MS;

  let socket: RelaySocket | null = null;
  let heartbeat: ReturnType<typeof setInterval> | null = null;
  let finished = false;

  return new ReadableStream<Uint8Array>({
    start(controller) {
      const write = (text: string): void => {
        if (finished) {
          return;
        }

        try {
          controller.enqueue(encoder.encode(text));
        } catch {
          // The browser went away between the check and the enqueue. Nothing to
          // do about it here; `stop` is what actually releases the socket, and
          // the abort listener below will call it.
        }
      };

      const send = (message: StreamMessage): void => {
        write(encodeStreamMessage(message));
      };

      /**
       * Ends the response exactly once.
       *
       * `finished` rather than relying on `controller.close()` being idempotent:
       * it is not, and the three ways this can be reached — upstream close,
       * upstream error, browser disconnect — can and do race each other.
       */
      const stop = (last: StreamMessage | null): void => {
        if (finished) {
          return;
        }

        if (last !== null) {
          send(last);
        }

        finished = true;

        if (heartbeat !== null) {
          clearInterval(heartbeat);
          heartbeat = null;
        }

        try {
          socket?.close(1000, "client disconnected");
        } catch {
          // Already closed, or never opened.
        }

        try {
          controller.close();
        } catch {
          // Already closed by the runtime.
        }
      };

      if (options.signal.aborted) {
        stop(null);

        return;
      }

      options.signal.addEventListener("abort", () => stop(null), { once: true });

      try {
        socket = openSocket(options.url, [
          SUBPROTOCOL,
          // The token is appended raw: a JWT's alphabet is a legal subprotocol
          // token, which is the property the server's `strings.CutPrefix`
          // relies on. Encoding it would be the bug.
          `${BEARER_SUBPROTOCOL_PREFIX}${options.accessToken}`,
        ]);
      } catch {
        stop({ t: "closed", code: null, reason: "could not open the realtime connection" });

        return;
      }

      socket.addEventListener("open", () => {
        // One board per stream. The connection could hold sixteen, but this
        // relay is opened by a board screen and closed when it unmounts, so a
        // second subscription would be a room nobody is looking at — and
        // "events for a board the user is not viewing must never be applied" is
        // much easier to guarantee by never subscribing to one.
        try {
          socket?.send(JSON.stringify({ type: "subscribe", board_id: options.boardId }));
        } catch {
          stop({ t: "closed", code: null, reason: "could not subscribe" });

          return;
        }

        send({ t: "open" });
      });

      socket.addEventListener("message", (event) => {
        if (typeof event.data !== "string") {
          // The server speaks JSON text frames and closes 1003 on anything
          // else, so this is unreachable; dropping it is still better than
          // relaying a Blob the browser would fail to parse.
          return;
        }

        let frame: unknown;

        try {
          frame = JSON.parse(event.data);
        } catch {
          return;
        }

        send({ t: "frame", frame });
      });

      socket.addEventListener("close", (event) => {
        stop({ t: "closed", code: event.code, reason: event.reason });
      });

      socket.addEventListener("error", () => {
        // `error` is always followed by `close` for a socket that opened, and is
        // the only signal for one that never did. `stop` is idempotent, so
        // letting both run is safe and losing neither is the point.
        stop({ t: "closed", code: null, reason: "the realtime connection failed" });
      });

      heartbeat = setInterval(() => write(STREAM_HEARTBEAT), heartbeatMs);

      // Node keeps the process alive for a pending interval. A stream that is
      // waiting on a quiet board must not be a reason a container refuses to
      // exit during a deploy.
      heartbeat.unref?.();
    },

    cancel() {
      finished = true;

      if (heartbeat !== null) {
        clearInterval(heartbeat);
        heartbeat = null;
      }

      try {
        socket?.close(1000, "client disconnected");
      } catch {
        // Already gone.
      }
    },
  });
}

function defaultOpenSocket(url: string, protocols: string[]): RelaySocket {
  return new WebSocket(url, protocols) as unknown as RelaySocket;
}
