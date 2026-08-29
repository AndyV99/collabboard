/**
 * The server-side relay: one WebSocket in, one event stream out.
 *
 * # The assertion this file exists for
 *
 * **The access token goes into the subprotocol offer and appears nowhere in
 * what the browser receives.** That is the entire resolution of the collision
 * between ADR 0007 ("the browser never holds a token") and the API's handshake
 * ("the token arrives in a `Sec-WebSocket-Protocol` offer, because a browser
 * cannot set a header"), and it is the one property whose regression would be a
 * security bug rather than a broken feature. It is asserted directly, against
 * the bytes, rather than inferred from the fact that the code does not look
 * like it leaks.
 *
 * Everything else here is ordinary relay behaviour, tested with a fake socket
 * so there is no network and no timing.
 */

import { describe, expect, it, vi } from "vitest";

import {
  DEFAULT_RELAY_BUFFER_BYTES,
  RELAY_BUFFER_ENV,
  relayBoardStream,
  relayBufferBytes,
  type RelaySocket,
} from "@/lib/realtime/relay";
import { apiWebSocketUrl, looksLikeUuid } from "@/lib/realtime/stream";

const BOARD = "11111111-1111-4111-8111-111111111111";
const TOKEN = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzb21lYm9keSJ9.c2lnbmF0dXJl";

type Handlers = {
  open?: () => void;
  message?: (event: { data: unknown }) => void;
  close?: (event: { code: number; reason: string }) => void;
  error?: () => void;
};

function fakeSocket() {
  const handlers: Handlers = {};
  const sent: string[] = [];
  const closes: Array<{ code?: number; reason?: string }> = [];

  const socket: RelaySocket = {
    send: (data) => sent.push(data),
    close: (code, reason) => closes.push({ code, reason }),
    addEventListener: ((type: keyof Handlers, handler: never) => {
      handlers[type] = handler;
    }) as RelaySocket["addEventListener"],
  };

  return { socket, handlers, sent, closes };
}

function start(options?: { failToOpen?: boolean }) {
  const fake = fakeSocket();
  const controller = new AbortController();
  const offered: { url?: string; protocols?: string[] } = {};

  const stream = relayBoardStream({
    boardId: BOARD,
    accessToken: TOKEN,
    url: "ws://api.test/api/v1/ws",
    signal: controller.signal,
    heartbeatMs: 10_000,
    openSocket: (url, protocols) => {
      offered.url = url;
      offered.protocols = protocols;

      if (options?.failToOpen === true) {
        throw new Error("nope");
      }

      return fake.socket;
    },
  });

  const reader = stream.getReader();
  const decoder = new TextDecoder();
  const seen: string[] = [];

  /** Reads whatever has been enqueued so far, without blocking on more. */
  const drain = async (expected: number): Promise<string[]> => {
    while (seen.length < expected) {
      const { done, value } = await reader.read();

      if (done) {
        break;
      }

      seen.push(decoder.decode(value));
    }

    return seen;
  };

  return { ...fake, controller, offered, drain, reader, seen };
}

describe("authenticating the handshake", () => {
  it("offers the token in the subprotocol, raw, after the documented prefix", () => {
    const relay = start();

    expect(relay.offered.protocols).toEqual([
      "collabboard.v1",
      `bearer.collabboard.v1.${TOKEN}`,
    ]);
  });

  it("offers the plain subprotocol too, which is the one the server echoes back", () => {
    // `selectSubprotocol` returns the first client offer that matches the
    // server's list, and the server's list is `["collabboard.v1"]`. Omitting it
    // loses the negotiation entirely.
    const relay = start();

    expect(relay.offered.protocols?.[0]).toBe("collabboard.v1");
  });

  it("does not encode the token, because a JWT is already a legal subprotocol token", () => {
    // The server uses `strings.CutPrefix` and passes the remainder straight to
    // `Bearer `. Percent-encoding it here would be the bug.
    const relay = start();

    expect(relay.offered.protocols?.[1]).toContain(TOKEN);
    expect(relay.offered.protocols?.[1]).not.toContain("%");
  });

  it("never writes the token into anything the browser receives", async () => {
    const relay = start();

    relay.handlers.open?.();
    relay.handlers.message?.({
      data: JSON.stringify({ type: "subscribed", board_id: BOARD }),
    });
    relay.handlers.close?.({ code: 4001, reason: "access token expired" });

    const written = (await relay.drain(4)).join("");

    expect(written).not.toContain(TOKEN);
    // A fragment too, so a partially-leaked token is caught as well.
    expect(written).not.toContain(TOKEN.slice(0, 20));
    expect(written).not.toContain("bearer.collabboard");
  });
});

describe("relaying", () => {
  it("subscribes to exactly one board as soon as the socket opens", () => {
    const relay = start();

    relay.handlers.open?.();

    expect(relay.sent).toEqual([
      JSON.stringify({ type: "subscribe", board_id: BOARD }),
    ]);
  });

  it("passes frames through untouched, so the browser parses them once", async () => {
    const relay = start();

    relay.handlers.open?.();
    relay.handlers.message?.({
      data: JSON.stringify({ type: "subscribed", board_id: BOARD }),
    });

    const written = (await relay.drain(2)).join("");

    expect(written).toContain(
      `data: ${JSON.stringify({ t: "frame", frame: { type: "subscribed", board_id: BOARD } })}`,
    );
  });

  it("reports the upstream close code, which is what the browser acts on", async () => {
    const relay = start();

    relay.handlers.open?.();
    relay.handlers.close?.({ code: 4003, reason: "membership revoked" });

    const written = (await relay.drain(2)).join("");

    expect(written).toContain('"t":"closed"');
    expect(written).toContain('"code":4003');
    expect(written).toContain("membership revoked");
  });

  it("reports a socket that never opened as a close with no code", async () => {
    const relay = start({ failToOpen: true });

    const written = (await relay.drain(1)).join("");

    expect(written).toContain('"code":null');
  });

  it("drops a frame that is not JSON rather than relaying nonsense", async () => {
    const relay = start();

    relay.handlers.open?.();
    relay.handlers.message?.({ data: "not json" });
    relay.handlers.message?.({ data: JSON.stringify({ type: "pong" }) });

    const written = (await relay.drain(2)).join("");

    expect(written).not.toContain("not json");
    expect(written).toContain("pong");
  });

  it("closes the upstream socket when the browser goes away", () => {
    const relay = start();

    relay.handlers.open?.();
    relay.controller.abort();

    expect(relay.closes).toHaveLength(1);
  });

  it("ends the response only once, however many things end it at the same time", async () => {
    // The upstream close, the upstream error and the browser disconnecting can
    // and do race; `controller.close()` is not idempotent.
    const relay = start();

    relay.handlers.open?.();
    relay.handlers.error?.();
    relay.handlers.close?.({ code: 1006, reason: "" });
    relay.controller.abort();

    const written = await relay.drain(3);

    expect(written.join("").match(/"t":"closed"/g)).toHaveLength(1);
  });

  it("never opens a socket at all when the browser has already gone", () => {
    const controller = new AbortController();

    controller.abort();

    const openSocket = vi.fn();

    relayBoardStream({
      boardId: BOARD,
      accessToken: TOKEN,
      url: "ws://api.test/api/v1/ws",
      signal: controller.signal,
      openSocket: openSocket as never,
    }).getReader();

    expect(openSocket).not.toHaveBeenCalled();
  });
});

describe("resolving the API's WebSocket URL", () => {
  it("derives ws from http and wss from https, rather than taking a second variable", () => {
    // Two environment variables naming one service is two things to get out of
    // step, and the failure when they disagree is a realtime path silently
    // pointed at the wrong environment while every REST call is correct.
    expect(apiWebSocketUrl("http://localhost:8080")).toBe(
      "ws://localhost:8080/api/v1/ws",
    );
    expect(apiWebSocketUrl("https://api.example.com")).toBe(
      "wss://api.example.com/api/v1/ws",
    );
  });

  it("does not double a slash on a base that has one", () => {
    expect(apiWebSocketUrl("https://api.example.com/")).toBe(
      "wss://api.example.com/api/v1/ws",
    );
  });
});

describe("refusing a board id before opening anything", () => {
  it("accepts a uuid and refuses everything else", () => {
    expect(looksLikeUuid(BOARD)).toBe(true);
    expect(looksLikeUuid("b-1")).toBe(false);
    expect(looksLikeUuid("../../auth/refresh")).toBe(false);
    expect(looksLikeUuid("")).toBe(false);
  });
});

/**
 * Backpressure (#99).
 *
 * The relay reads the upstream socket as fast as the API produces frames,
 * whether or not the browser is reading — that is what makes it a relay. Before
 * this bound, a browser that stopped reading did not slow it down; it made this
 * process hold every frame for it, in memory, without limit. A backgrounded tab
 * whose socket is throttled, a suspended laptop, a congested link: all of them.
 *
 * The API bounds exactly this for its own connections (REALTIME_SEND_BUFFER,
 * 4002) and that protects the API, not the web tier — behind the relay, the
 * API's consumer is always reading.
 */
describe("a browser that stops reading", () => {
  /** Starts a relay whose stream is never read from. */
  function startUnread(maxBufferedBytes: number) {
    const fake = fakeSocket();
    const controller = new AbortController();

    const stream = relayBoardStream({
      boardId: BOARD,
      accessToken: TOKEN,
      url: "ws://api.test/api/v1/ws",
      signal: controller.signal,
      heartbeatMs: 10_000,
      maxBufferedBytes,
      openSocket: () => fake.socket,
    });

    return { ...fake, stream, controller };
  }

  /** One frame whose encoded size is comfortably known. */
  function pushFrame(handlers: Handlers, index: number): void {
    handlers.message?.({
      data: JSON.stringify({
        type: "card.updated",
        payload: "x".repeat(200),
        index,
      }),
    });
  }

  it("is dropped once it is far enough behind, instead of buffering forever", async () => {
    const relay = startUnread(1000);

    relay.handlers.open?.();

    // Nothing reads `relay.stream`. Every frame accumulates.
    for (let i = 0; i < 50; i++) {
      pushFrame(relay.handlers, i);
    }

    // The upstream socket is released rather than left draining into a stream
    // nobody is taking from — which is the memory that was leaking.
    expect(relay.closes.length).toBeGreaterThan(0);

    // And the last thing written is the close, so the browser is told rather
    // than left to infer it from a stream that simply stops.
    const reader = relay.stream.getReader();
    const decoder = new TextDecoder();
    const messages: string[] = [];

    for (;;) {
      const { done, value } = await reader.read();

      if (done) {
        break;
      }

      messages.push(decoder.decode(value));
    }

    const last = messages[messages.length - 1] ?? "";

    expect(last).toContain(`"t":"closed"`);
    expect(last).toContain(`"code":4002`);
  });

  // The bound has to be a bound, not merely an eventual stop: the whole point
  // is that the queue does not grow with the number of frames the API sends.
  it("stops accumulating rather than growing with the flood", async () => {
    const small = startUnread(1000);

    small.handlers.open?.();

    for (let i = 0; i < 5000; i++) {
      pushFrame(small.handlers, i);
    }

    const reader = small.stream.getReader();
    const decoder = new TextDecoder();
    let buffered = 0;

    for (;;) {
      const { done, value } = await reader.read();

      if (done) {
        break;
      }

      buffered += decoder.decode(value).length;
    }

    // 5000 frames of ~250 bytes would be well over a megabyte unbounded. The
    // ceiling is the budget plus the one frame that crossed it plus the close.
    expect(buffered).toBeLessThan(1000 * 3);
  });

  // A reading browser must be untouched by any of this. Without this the fix
  // could "pass" by shedding everybody.
  it("does not drop a browser that is keeping up", async () => {
    const relay = start();

    relay.handlers.open?.();

    for (let i = 0; i < 200; i++) {
      relay.handlers.message?.({
        data: JSON.stringify({ type: "card.updated", index: i }),
      });
      // Read as we go, which is what a live browser does.
      await relay.drain(relay.seen.length + 1);
    }

    const everything = relay.seen.join("");

    expect(everything).not.toContain(`"code":4002`);
    expect(relay.closes).toHaveLength(0);
  });
});

describe("the relay buffer setting", () => {
  it("defaults when unset", () => {
    expect(relayBufferBytes({})).toBe(DEFAULT_RELAY_BUFFER_BYTES);
    expect(relayBufferBytes({ [RELAY_BUFFER_ENV]: "  " })).toBe(
      DEFAULT_RELAY_BUFFER_BYTES,
    );
  });

  it("takes a usable value", () => {
    expect(relayBufferBytes({ [RELAY_BUFFER_ENV]: "4096" })).toBe(4096);
  });

  // A zero or negative budget sheds every viewer on their first frame, which is
  // a denial of service written as a configuration value. `Number("")` is 0 and
  // `Number("1MB")` is NaN, so both shapes have to be caught.
  it.each(["0", "-1", "1MB", "abc", "1.5", ""])(
    "falls back rather than accepting %s",
    (value) => {
      expect(relayBufferBytes({ [RELAY_BUFFER_ENV]: value })).toBe(
        DEFAULT_RELAY_BUFFER_BYTES,
      );
    },
  );
});
