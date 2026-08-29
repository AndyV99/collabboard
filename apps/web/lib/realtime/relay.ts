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

import { logEvent } from "@/lib/log";

import { BEARER_SUBPROTOCOL_PREFIX, SUBPROTOCOL } from "./protocol";
import { CLOSE_SLOW_CONSUMER } from "./recovery";
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
  addEventListener(
    type: "message",
    handler: (event: { data: unknown }) => void,
  ): void;
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
  /** See {@link DEFAULT_RELAY_BUFFER_BYTES}. */
  maxBufferedBytes?: number;
};

/**
 * How many bytes may sit unread in one browser's stream before the relay gives
 * up on it.
 *
 * This is the web tier's half of a bound the API already has. `apps/api` caps a
 * WebSocket connection at REALTIME_SEND_BUFFER frames (64) and closes an
 * overrunning one with 4002; that protects the API from a slow consumer and
 * does nothing for this process, because the relay reads the socket as fast as
 * the API produces regardless of whether the browser is reading. Without a
 * bound here, backpressure has nowhere to go except this heap.
 *
 * A byte budget rather than a frame count, even though the API counts frames:
 * the thing at risk here is memory, and an event payload is capped at 64 KiB by
 * the API's publisher, so 64 frames is anywhere between a few hundred bytes and
 * 4 MiB. Counting the thing that actually runs out avoids that spread.
 *
 * 1 MiB is generous for a live board -- a reading browser never accumulates,
 * because the runtime pulls as it writes to the socket, so this is only ever
 * reached by a consumer that has genuinely stopped: a backgrounded tab whose
 * socket is throttled, a suspended laptop, a congested link.
 */
export const DEFAULT_RELAY_BUFFER_BYTES = 1024 * 1024;

/** The environment variable that overrides it. */
export const RELAY_BUFFER_ENV = "REALTIME_RELAY_BUFFER_BYTES";

/**
 * Resolves the budget from the environment, read at request time.
 *
 * Read at request time rather than at module load, for the reason `apiBaseUrl`
 * gives: Next inlines `NEXT_PUBLIC_*` at build time, and a value baked into the
 * image is a value that cannot be tuned without a rebuild -- which is exactly
 * what #16 removed and what the Dockerfiles' "could this image be promoted
 * dev -> staging -> prod unchanged?" test forbids.
 *
 * An unusable value falls back to the default and says so, rather than
 * throwing. This is a tuning knob on a request path, not a security boundary:
 * refusing to serve realtime because somebody typed `1MB` would be a worse
 * failure than serving it with the default and a warning. That is the opposite
 * of the call made for HTTP_TRUSTED_PROXIES in the API, and deliberately so --
 * that one decides who may assert a client's identity.
 */
export function relayBufferBytes(
  env: Readonly<Record<string, string | undefined>> = process.env,
): number {
  const raw = env[RELAY_BUFFER_ENV];

  if (raw === undefined || raw.trim() === "") {
    return DEFAULT_RELAY_BUFFER_BYTES;
  }

  const parsed = Number(raw.trim());

  // Number("") is 0 and Number("12kb") is NaN; a zero or negative budget would
  // shed every viewer on their first frame, which is a denial of service
  // written as a configuration value.
  if (!Number.isSafeInteger(parsed) || parsed <= 0) {
    logEvent("warn", "ignoring an unusable realtime relay buffer setting", {
      event: "web.realtime.relay_buffer_invalid",
      value: raw,
      using: DEFAULT_RELAY_BUFFER_BYTES,
    });

    return DEFAULT_RELAY_BUFFER_BYTES;
  }

  return parsed;
}

/**
 * Opens the upstream socket and returns the SSE body for the browser.
 *
 * The stream ends when the upstream socket closes, and never on its own. A
 * relay that ended the response early would look to the browser exactly like a
 * dropped connection, which it would then reconnect and re-fetch for — correct,
 * but a re-fetch nobody needed.
 */
export function relayBoardStream(
  options: RelayOptions,
): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  const openSocket = options.openSocket ?? defaultOpenSocket;
  const heartbeatMs = options.heartbeatMs ?? STREAM_HEARTBEAT_MS;
  const maxBufferedBytes =
    options.maxBufferedBytes ?? DEFAULT_RELAY_BUFFER_BYTES;

  let socket: RelaySocket | null = null;
  let heartbeat: ReturnType<typeof setInterval> | null = null;
  let finished = false;

  return new ReadableStream<Uint8Array>(
    {
      start(controller) {
        /**
         * Puts bytes on the stream, bypassing the budget below.
         *
         * Used for the final message of a stream that is ending anyway, where
         * refusing to write is worse than one more small enqueue: without it, a
         * browser shed for backpressure would be told nothing and would have to
         * infer the close from the stream simply stopping.
         */
        const enqueue = (text: string): void => {
          try {
            controller.enqueue(encoder.encode(text));
          } catch {
            // The browser went away between the check and the enqueue. Nothing to
            // do about it here; `stop` is what actually releases the socket, and
            // the abort listener below will call it.
          }
        };

        /**
         * Whether the browser is far enough behind to give up on.
         *
         * `desiredSize` is the runtime's own accounting under the
         * ByteLengthQueuingStrategy this stream is constructed with: it starts at
         * maxBufferedBytes and falls by the byte length of every chunk that is
         * enqueued and not yet read. At or below zero, that many bytes are
         * sitting in this process for a browser that is not taking them.
         *
         * It is null only once the stream has errored, which `finished` and the
         * try/catch in `enqueue` already cover.
         */
        const overBudget = (): boolean =>
          controller.desiredSize !== null && controller.desiredSize <= 0;

        const write = (text: string): void => {
          if (finished) {
            return;
          }

          if (overBudget()) {
            shedSlowConsumer();

            return;
          }

          enqueue(text);
        };

        const send = (message: StreamMessage): void => {
          write(encodeStreamMessage(message));
        };

        /**
         * Ends a stream nobody is reading.
         *
         * 4002 is reused rather than invented, and it is not a white lie: the
         * browser client's `recovery.ts` already treats it as "you fell behind,
         * so you missed events -- back off, reconnect, re-subscribe, re-fetch",
         * and every clause of that is true here. The only difference from the
         * API's own 4002 is which process ran out of patience. Inventing a
         * fourth code would mean a new branch in a client that already does
         * exactly the right thing.
         */
        const shedSlowConsumer = (): void => {
          logEvent(
            "warn",
            "realtime relay dropped a viewer that stopped reading",
            {
              event: "web.realtime.relay_slow_consumer",
              board_id: options.boardId,
              max_buffered_bytes: maxBufferedBytes,
            },
          );

          stop({
            t: "closed",
            code: CLOSE_SLOW_CONSUMER,
            reason: "the browser stopped reading this stream",
          });
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

          // `enqueue` and not `send`: this stream is ending, so the budget check
          // in `write` must not apply -- and, more sharply, `shedSlowConsumer`
          // calls this function, so routing the final message through `write`
          // would recurse the moment the budget is what triggered the stop.
          if (last !== null) {
            enqueue(encodeStreamMessage(last));
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

        options.signal.addEventListener("abort", () => stop(null), {
          once: true,
        });

        try {
          socket = openSocket(options.url, [
            SUBPROTOCOL,
            // The token is appended raw: a JWT's alphabet is a legal subprotocol
            // token, which is the property the server's `strings.CutPrefix`
            // relies on. Encoding it would be the bug.
            `${BEARER_SUBPROTOCOL_PREFIX}${options.accessToken}`,
          ]);
        } catch {
          stop({
            t: "closed",
            code: null,
            reason: "could not open the realtime connection",
          });

          return;
        }

        socket.addEventListener("open", () => {
          // One board per stream. The connection could hold sixteen, but this
          // relay is opened by a board screen and closed when it unmounts, so a
          // second subscription would be a room nobody is looking at — and
          // "events for a board the user is not viewing must never be applied" is
          // much easier to guarantee by never subscribing to one.
          try {
            socket?.send(
              JSON.stringify({ type: "subscribe", board_id: options.boardId }),
            );
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
          stop({
            t: "closed",
            code: null,
            reason: "the realtime connection failed",
          });
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
    },
    // The strategy is what makes `desiredSize` mean bytes rather than chunks.
    // With the default (a count strategy at a high-water mark of 1) it would go
    // negative after the second unread message regardless of size, which is far
    // too eager -- a browser is allowed to be a frame or two behind.
    new ByteLengthQueuingStrategy({ highWaterMark: maxBufferedBytes }),
  );
}

function defaultOpenSocket(url: string, protocols: string[]): RelaySocket {
  return new WebSocket(url, protocols) as unknown as RelaySocket;
}
