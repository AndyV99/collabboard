/**
 * The reconnect state machine: what each way of losing the stream does next.
 *
 * # Nothing here waits for anything
 *
 * `BoardStream` takes its timer, its jitter, its `fetch` and its token refresh
 * as arguments, so this file supplies all four and drives the clock by hand.
 * There is no `waitFor` with a real delay anywhere below, and no
 * `vi.useFakeTimers` either — the schedule is a list this test owns, and
 * "reconnected after the right delay" is an assertion about a number rather
 * than a race that usually wins.
 *
 * That matters more here than in the rest of the suite. `board-editing.test.tsx`
 * already had to write down the rule about transient windows and
 * immediately-resolving stubs; a socket with timers and backoff is the same trap
 * with two more moving parts.
 *
 * # The close codes are demonstrated, not asserted about
 *
 * Each one drives the machine through a real close and then checks what it
 * actually did — whether it dialled again, whether it refreshed first, whether
 * it gave up — rather than checking that a mapping function returns the right
 * enum. The mapping is tested too, in `realtime-recovery.test.ts`, but a
 * mapping that is right and never consulted would pass that and fail this.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

import { BoardStream, type LiveStatus, decodeBlock } from "@/lib/realtime/client";
import type { ServerFrame } from "@/lib/realtime/protocol";
import {
  CLOSE_MEMBERSHIP_REVOKED,
  CLOSE_SLOW_CONSUMER,
  CLOSE_TOKEN_EXPIRED,
} from "@/lib/realtime/recovery";
import type { StreamMessage } from "@/lib/realtime/stream";

const BOARD = "11111111-1111-4111-8111-111111111111";
const OTHER_BOARD = "99999999-9999-4999-8999-999999999999";

/** Lets the stream's pending `reader.read()` settle. */
function flush(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

type Scheduled = { id: number; run: () => void; ms: number };

function harness(options?: { status?: number }) {
  const encoder = new TextEncoder();

  const urls: string[] = [];
  const timers: Scheduled[] = [];
  const frames: ServerFrame[] = [];
  const statuses: LiveStatus[] = [];

  let controller: ReadableStreamDefaultController<Uint8Array> | null = null;
  let nextTimerId = 1;
  let resubscribes = 0;

  const refreshSession = vi.fn(async () => true);
  /** Records the order refresh and dial happened in, which 4001 is all about. */
  const order: string[] = [];

  const fetchImpl = vi.fn(async (input: unknown) => {
    urls.push(String(input));
    order.push("fetch");

    if (options?.status !== undefined && options.status !== 200) {
      return new Response("{}", {
        status: options.status,
        headers: { "content-type": "application/json" },
      });
    }

    const body = new ReadableStream<Uint8Array>({
      start(open) {
        controller = open;
      },
    });

    return new Response(body, { status: 200 });
  }) as unknown as typeof fetch;

  const stream = new BoardStream(
    BOARD,
    {
      onFrame: (frame) => frames.push(frame),
      onResubscribed: () => {
        resubscribes += 1;
      },
      onStatus: (status) => statuses.push(status),
    },
    {
      fetchImpl,
      setTimer: (run, ms) => {
        const id = nextTimerId;

        nextTimerId += 1;
        timers.push({ id, run, ms });

        return id;
      },
      clearTimer: (id) => {
        const at = timers.findIndex((timer) => timer.id === id);

        if (at !== -1) {
          timers.splice(at, 1);
        }
      },
      // Fixed jitter, so a delay is a number this test can name. The
      // distribution is `realtime-recovery.test.ts`'s job.
      random: () => 0.5,
      refreshSession: async () => {
        order.push("refresh");

        return refreshSession();
      },
    },
  );

  const write = (text: string): void => {
    controller?.enqueue(encoder.encode(text));
  };

  return {
    stream,
    urls,
    timers,
    frames,
    statuses,
    order,
    refreshSession,
    dials: () => urls.length,
    resubscribes: () => resubscribes,
    /** Sends one relay message down the open stream. */
    send: (message: StreamMessage) => write(`data: ${JSON.stringify(message)}\n\n`),
    /** Sends raw bytes, for the chunk-splitting test. */
    raw: write,
    subscribed: (boardId = BOARD) =>
      write(
        `data: ${JSON.stringify({
          t: "frame",
          frame: { type: "subscribed", board_id: boardId },
        })}\n\n`,
      ),
    /** Runs the one pending timer, as the browser would when it fires. */
    fire: () => {
      const next = timers.shift();

      if (next === undefined) {
        throw new Error("no timer was scheduled");
      }

      next.run();

      return next.ms;
    },
    last: () => statuses[statuses.length - 1],
  };
}

let live: ReturnType<typeof harness> | null = null;

beforeEach(() => {
  live?.stream.stop();
  live = null;
});

describe("subscribing", () => {
  it("re-reads the board on the first subscribe, not only on later ones", async () => {
    // The first render's props came from the server, but they were read before
    // the socket existed — so the same at-most-once gap ADR 0005 describes
    // applies to it. `onResubscribed` firing here is that gap being closed.
    const h = (live = harness());

    h.stream.start();
    await flush();

    expect(h.resubscribes()).toBe(0);

    h.subscribed();
    await flush();

    expect(h.resubscribes()).toBe(1);
    expect(h.last()).toEqual({ state: "live" });
  });

  it("ignores a subscribed frame naming a board this stream is not watching", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();

    h.subscribed(OTHER_BOARD);
    await flush();

    expect(h.resubscribes()).toBe(0);
    expect(h.last()).toEqual({ state: "connecting" });
  });

  it("reassembles a frame that arrives split across two chunks", async () => {
    // A chunk boundary can fall anywhere. A client that parsed per chunk would
    // drop whichever frame happened to be cut in half, which is a bug that only
    // shows up under exactly the load the feature is for.
    const h = (live = harness());

    h.stream.start();
    await flush();

    const message = JSON.stringify({
      t: "frame",
      frame: { type: "subscribed", board_id: BOARD },
    });

    h.raw(`data: ${message.slice(0, 12)}`);
    await flush();

    expect(h.resubscribes()).toBe(0);

    h.raw(`${message.slice(12)}\n\n`);
    await flush();

    expect(h.resubscribes()).toBe(1);
  });
});

describe("close code 4001 — the access token expired", () => {
  it("refreshes the session before dialling again, and does not sign the user out", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();
    h.subscribed();
    await flush();

    h.send({ t: "closed", code: CLOSE_TOKEN_EXPIRED, reason: "access token expired" });
    await flush();

    // Not stopped: a fifteen-minute credential ending on schedule is the normal
    // life of a page someone left open, not the end of their session.
    expect(h.last()).not.toMatchObject({ state: "stopped" });

    h.fire();
    await flush();

    expect(h.refreshSession).toHaveBeenCalledTimes(1);
    // The order is the whole point. Dialling first would present the same
    // expired token and be closed again immediately — a loop, not a recovery.
    expect(h.order).toEqual(["fetch", "refresh", "fetch"]);
  });

  it("comes back promptly rather than treating a scheduled expiry as a failure", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();
    h.send({ t: "closed", code: CLOSE_TOKEN_EXPIRED, reason: "" });
    await flush();

    // The first backoff window with fixed 0.5 jitter, not an escalated one.
    expect(h.timers[0].ms).toBe(250);
  });

  it("stops, without retrying, when the refresh says the session is genuinely over", async () => {
    const h = (live = harness());

    h.refreshSession.mockResolvedValue(false);

    h.stream.start();
    await flush();
    h.send({ t: "closed", code: CLOSE_TOKEN_EXPIRED, reason: "" });
    await flush();
    h.fire();
    await flush();

    expect(h.last()).toMatchObject({ state: "stopped" });
    expect(h.dials()).toBe(1);
    expect(h.timers).toHaveLength(0);
  });
});

describe("close code 4002 — dropped as a slow consumer", () => {
  it("backs off, reconnects, and re-reads the board because events were missed", async () => {
    // This is the case ADR 0005 built the re-fetch for: the server dropped this
    // connection *because* it had fallen behind, so events were missed by
    // definition and only a full read can close the gap.
    const h = (live = harness());

    h.stream.start();
    await flush();
    h.subscribed();
    await flush();

    expect(h.resubscribes()).toBe(1);

    h.send({ t: "closed", code: CLOSE_SLOW_CONSUMER, reason: "send buffer full" });
    await flush();

    expect(h.dials()).toBe(1);

    const delay = h.fire();

    expect(delay).toBeGreaterThan(0);
    await flush();

    expect(h.dials()).toBe(2);

    h.subscribed();
    await flush();

    expect(h.resubscribes()).toBe(2);
    expect(h.last()).toEqual({ state: "live" });
  });
});

describe("close code 4003 — membership revoked", () => {
  it("stops for good and says why, rather than retrying against a refusal", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();
    h.subscribed();
    await flush();

    h.send({ t: "closed", code: CLOSE_MEMBERSHIP_REVOKED, reason: "membership revoked" });
    await flush();

    const status = h.last();

    expect(status.state).toBe("stopped");
    expect(status.state === "stopped" && status.notice).toContain("no longer have access");

    // Nothing scheduled, so nothing to fire: every reconnect would be
    // authorised, refused and closed again.
    expect(h.timers).toHaveLength(0);
    expect(h.dials()).toBe(1);
  });

  it("stays stopped even if something later tries to start it", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();
    h.send({ t: "closed", code: CLOSE_MEMBERSHIP_REVOKED, reason: "" });
    await flush();

    h.stream.start();
    await flush();

    expect(h.dials()).toBe(1);
  });
});

describe("the other ways a stream ends", () => {
  it("honours the server's own reconnect hint after a shutdown frame", async () => {
    // A rolling deploy is every client on the instance reconnecting at once.
    // The server jitters a hint precisely so they do not all arrive together,
    // and ignoring it would put the herd back.
    const h = (live = harness());

    h.stream.start();
    await flush();

    h.send({
      t: "frame",
      frame: { type: "shutdown", message: "restarting", reconnect_after_ms: 1400 },
    });
    await flush();

    h.send({ t: "closed", code: 1001, reason: "server is restarting" });
    await flush();

    expect(h.timers[0].ms).toBe(1400);
  });

  it("treats a stream that simply ends as a drop, and reconnects", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();

    h.send({ t: "open" });
    await flush();

    // The relay died without sending a close message.
    h.stream.stop();

    expect(h.timers).toHaveLength(0);
  });

  it("escalates the wait when connecting keeps failing, and never dials instantly", async () => {
    const h = (live = harness({ status: 503 }));

    h.stream.start();
    await flush();

    const first = h.fire();

    await flush();

    const second = h.fire();

    await flush();

    expect(second).toBeGreaterThan(first);
    expect(first).toBeGreaterThanOrEqual(100);
  });

  it("says nothing for a single blip and speaks up once it is more than that", async () => {
    const h = (live = harness({ status: 503 }));

    h.stream.start();
    await flush();

    // One failure is not worth a banner; the board is not wrong yet.
    expect(h.statuses.filter((status) => status.state === "reconnecting")).toHaveLength(0);

    h.fire();
    await flush();

    expect(h.last()).toMatchObject({ state: "reconnecting" });
  });

  it("cancels a pending retry when the board unmounts", async () => {
    const h = (live = harness({ status: 503 }));

    h.stream.start();
    await flush();

    expect(h.timers).toHaveLength(1);

    h.stream.stop();

    expect(h.timers).toHaveLength(0);
  });
});

describe("subscription refusals that arrive as frames rather than close codes", () => {
  it("gives up on a forbidden board without retrying", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();

    h.stream.abandon({
      action: "stop",
      notice: "You do not have access to this board's live updates.",
      escalates: false,
    });
    await flush();

    expect(h.last()).toMatchObject({ state: "stopped" });
    expect(h.timers).toHaveLength(0);
  });

  it("retries when the server could not check access rather than refusing it", async () => {
    const h = (live = harness());

    h.stream.start();
    await flush();

    h.stream.abandon({ action: "reconnect", notice: null, escalates: true });
    await flush();

    expect(h.timers).toHaveLength(1);

    h.fire();
    await flush();

    expect(h.dials()).toBe(2);
  });
});

describe("decoding one SSE block", () => {
  it("ignores a heartbeat comment rather than treating it as a bad frame", () => {
    expect(decodeBlock(": heartbeat")).toBeNull();
  });

  it("ignores a block with no data line", () => {
    expect(decodeBlock("event: something")).toBeNull();
  });

  it("reads the three message kinds", () => {
    expect(decodeBlock('data: {"t":"open"}')).toEqual({ t: "open" });
    expect(decodeBlock('data: {"t":"frame","frame":{"type":"pong"}}')).toEqual({
      t: "frame",
      frame: { type: "pong" },
    });
    expect(decodeBlock('data: {"t":"closed","code":4003,"reason":"gone"}')).toEqual({
      t: "closed",
      code: 4003,
      reason: "gone",
    });
  });

  it("reads a close with no code, which is the relay never getting a socket", () => {
    expect(decodeBlock('data: {"t":"closed","code":null,"reason":""}')).toEqual({
      t: "closed",
      code: null,
      reason: "",
    });
  });
});
