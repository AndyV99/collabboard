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

import { relayBoardStream, type RelaySocket } from "@/lib/realtime/relay";
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

    expect(relay.sent).toEqual([JSON.stringify({ type: "subscribe", board_id: BOARD })]);
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
    expect(apiWebSocketUrl("http://localhost:8080")).toBe("ws://localhost:8080/api/v1/ws");
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
