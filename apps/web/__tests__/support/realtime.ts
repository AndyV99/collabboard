/**
 * A fake realtime stream, for any test that renders a board.
 *
 * Since #66 a mounted `BoardView` opens `GET /api/realtime/boards/:id`. Every
 * board test therefore has to answer that request, and the two that already
 * existed had to keep working unchanged — they assert on `fetch.mock.calls[0]`,
 * and a realtime request arriving first would have renumbered every one of them.
 *
 * So {@link withRealtime} *wraps* a stub rather than replacing it: the realtime
 * request is answered here and never reaches the stub, which therefore sees
 * exactly the calls it saw before this issue. The alternative — teaching thirty
 * assertions to skip a call — would have made every one of them describe the
 * test harness instead of the behaviour.
 *
 * # No timers, no sockets, no waiting
 *
 * The stream is a `ReadableStream` this file enqueues into directly, so a test
 * says "the server sent this event" and the next assertion can be about what
 * the board did. There is nothing to poll and no delay to tune, which is the
 * rule `board-editing.test.tsx`'s header lays down: a transient window asserted
 * against an immediately-resolving stub is a race, and realtime adds sockets
 * and timers to a file that already had transitions.
 */

import type { StreamMessage } from "@/lib/realtime/stream";

/**
 * The shape the board tests already use for a `fetch` stub.
 *
 * Matched exactly rather than widened to `unknown`: a parameter typed
 * `unknown` is not assignable *from* one typed `string` under
 * `strictFunctionTypes`, so widening here would push a cast into every call
 * site — which is the opposite of what a helper is for.
 */
type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export type FakeRealtime = {
  /** Install this as the global `fetch`. */
  fetch: FetchLike;
  /** How many streams have been opened, including closed ones. */
  opened: () => number;
  /** Pushes one message down the open stream, as the relay would. */
  send: (message: StreamMessage) => void;
  /** The `{"type":"subscribed"}` frame, which is what triggers the re-fetch. */
  subscribe: (boardId: string) => void;
  /** Relays a frame from the Go API. */
  frame: (frame: unknown) => void;
  /** Ends the stream with an upstream close code, then lets it be reopened. */
  close: (code: number | null, reason?: string) => void;
  /** Ends the stream *without* a close message — a relay that simply died. */
  drop: () => void;
};

/** Matches the path `lib/realtime/stream.ts` builds. */
function isRealtimePath(input: string): boolean {
  return input.startsWith("/api/realtime/boards/");
}

/**
 * Wraps a `fetch` stub so realtime requests are answered by a fake stream.
 *
 * `delegate` receives every other request untouched — same arguments, same
 * call indices, same recorded body.
 */
export function fakeRealtime(delegate?: FetchLike, options?: { status?: number }): FakeRealtime {
  const encoder = new TextEncoder();

  let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
  let opened = 0;

  const write = (text: string): void => {
    if (controller === null) {
      return;
    }

    try {
      controller.enqueue(encoder.encode(text));
    } catch {
      // Closed under us; the client treats that as a drop, which is the truth.
    }
  };

  const send = (message: StreamMessage): void => {
    write(`data: ${JSON.stringify(message)}\n\n`);
  };

  const end = (): void => {
    const open = controller;

    controller = null;

    try {
      open?.close();
    } catch {
      // Already closed.
    }
  };

  return {
    opened: () => opened,

    send,

    frame: (frame) => send({ t: "frame", frame }),

    subscribe: (boardId) =>
      send({ t: "frame", frame: { type: "subscribed", board_id: boardId } }),

    close: (code, reason = "") => {
      send({ t: "closed", code, reason });
      end();
    },

    drop: end,

    fetch: async (input, init) => {
      if (!isRealtimePath(input)) {
        if (delegate === undefined) {
          throw new Error(`unexpected fetch: ${String(input)}`);
        }

        return delegate(input, init);
      }

      opened += 1;

      const status = options?.status ?? 200;

      if (status !== 200) {
        return new Response(JSON.stringify({ error: "no" }), {
          status,
          headers: { "content-type": "application/json" },
        });
      }

      const body = new ReadableStream<Uint8Array>({
        start(open) {
          controller = open;
          open.enqueue(encoder.encode(`data: ${JSON.stringify({ t: "open" })}\n\n`));
        },
        cancel() {
          controller = null;
        },
      });

      return new Response(body, {
        status: 200,
        headers: { "content-type": "text/event-stream" },
      });
    },
  };
}

/** The common case: only the stream needs faking, and nothing drives it. */
export function withRealtime(delegate: FetchLike): FetchLike {
  return fakeRealtime(delegate).fetch;
}
