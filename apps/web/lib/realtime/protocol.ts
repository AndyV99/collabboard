/**
 * The realtime wire format, as data.
 *
 * `apps/api/internal/realtime/event.go` defines six frame types and ten event
 * types. This file is the only place in the web app that knows their names: it
 * turns an unknown JSON value into a closed union, and refuses anything it does
 * not recognise rather than letting a half-understood frame reach the board.
 *
 * Everything here is pure. No socket, no clock, no React — which is what lets
 * the whole protocol be tested as a table of inputs and expected outputs
 * instead of by provoking a server.
 *
 * # Why the payload parsers are the REST ones
 *
 * ADR 0005 chose to publish "the *same* object representation the REST
 * endpoints return — `cardBody`, `columnBody`, `boardBody` — so a client has
 * one card type and one decoder". Reusing {@link parseCard} and
 * {@link parseColumn} is that decision being taken up rather than a shortcut: a
 * field added to `GET /cards/:id` and forgotten in the event payload becomes a
 * parse failure here, which is exactly the drift the ADR was protecting
 * against.
 */

import {
  type Card,
  type Column,
  parseCard,
  parseColumn,
  unwrap,
} from "@/lib/api/types";

/** The `event.type` strings the API publishes (`internal/api/events.go`). */
export type RealtimeEventType =
  | "card.created"
  | "card.updated"
  | "card.moved"
  | "card.deleted"
  | "column.created"
  | "column.updated"
  | "column.moved"
  | "column.deleted"
  | "board.updated"
  | "board.deleted";

/**
 * One event, with its payload already decoded into the app's own types.
 *
 * `id` is the server's per-event uuid. It is carried because it is the only
 * thing that makes a duplicate detectable: the stream is at-most-once, but a
 * reconnect can re-deliver nothing *and* a re-fetch can overlap an event, so
 * "have I already applied this one" has to be answerable by identity rather
 * than by comparing boards.
 */
export type RealtimeEvent = {
  id: string;
  boardId: string;
  actorId: string;
  occurredAt: string;
} & (
  | { type: "card.created"; card: Card }
  | { type: "card.updated"; card: Card }
  | { type: "card.moved"; card: Card; fromColumnId: string; afterCardId: string | null }
  | { type: "card.deleted"; cardId: string; columnId: string }
  | { type: "column.created"; column: Column }
  | { type: "column.updated"; column: Column }
  | { type: "column.moved"; column: Column; afterColumnId: string | null }
  | { type: "column.deleted"; columnId: string }
  | { type: "board.updated"; boardId: string }
  | { type: "board.deleted"; boardId: string }
);

/**
 * A frame from the server, once understood.
 *
 * `unknown` is a real member rather than a parse failure. The API may add a
 * frame type, and a client that threw on one it had not been taught would break
 * on a deploy it was supposed to be indifferent to. An unknown frame is ignored
 * and counted, not fatal.
 */
export type ServerFrame =
  | { kind: "event"; event: RealtimeEvent }
  | { kind: "subscribed"; boardId: string }
  | { kind: "unsubscribed"; boardId: string; reason: string | null }
  | { kind: "error"; boardId: string | null; reason: string; message: string }
  | { kind: "shutdown"; message: string; reconnectAfterMs: number }
  | { kind: "pong" }
  | { kind: "unknown" };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function str(value: unknown, key: string): string | null {
  if (!isRecord(value)) {
    return null;
  }

  const found = value[key];

  return typeof found === "string" && found !== "" ? found : null;
}

/**
 * `after_card_id` / `after_column_id`, which are explicitly `null` for "first".
 *
 * The server never omits them (`*string` without `omitempty`), so absent and
 * null are distinguishable on the wire. They are treated the same here anyway:
 * "first" is the only sensible reading of both, and a client that refused an
 * absent anchor would be stricter than the contract for no benefit.
 */
function anchor(value: unknown, key: string): string | null {
  if (!isRecord(value)) {
    return null;
  }

  const found = value[key];

  return typeof found === "string" && found !== "" ? found : null;
}

/**
 * Parses one frame of JSON text.
 *
 * Returns null only for text that is not JSON at all. Everything else lands on
 * a member of {@link ServerFrame}, `unknown` included.
 */
export function parseFrame(text: string): ServerFrame | null {
  let value: unknown;

  try {
    value = JSON.parse(text);
  } catch {
    return null;
  }

  return frameFrom(value);
}

/** The parsed-value half of {@link parseFrame}, separated for testing. */
export function frameFrom(value: unknown): ServerFrame {
  if (!isRecord(value)) {
    return { kind: "unknown" };
  }

  switch (value.type) {
    case "event": {
      const event = eventFrom(value.event, str(value, "board_id"));

      return event === null ? { kind: "unknown" } : { kind: "event", event };
    }

    case "subscribed": {
      const boardId = str(value, "board_id");

      return boardId === null ? { kind: "unknown" } : { kind: "subscribed", boardId };
    }

    case "unsubscribed": {
      const boardId = str(value, "board_id");

      return boardId === null
        ? { kind: "unknown" }
        : { kind: "unsubscribed", boardId, reason: str(value, "reason") };
    }

    case "error":
      return {
        kind: "error",
        boardId: str(value, "board_id"),
        reason: str(value, "reason") ?? "unknown",
        message: str(value, "message") ?? "",
      };

    case "shutdown": {
      const hint = value.reconnect_after_ms;

      return {
        kind: "shutdown",
        message: str(value, "message") ?? "",
        reconnectAfterMs: typeof hint === "number" && hint > 0 ? hint : 0,
      };
    }

    case "pong":
      return { kind: "pong" };

    default:
      return { kind: "unknown" };
  }
}

/**
 * Decodes the inner event object.
 *
 * The board id comes from the *frame*, not from the payload. That is not
 * arbitrary: `internal/realtime/event.go` re-checks every envelope's board id
 * against the Redis channel it arrived on and drops a mismatch, so the frame's
 * copy is the one the server has vouched for. A payload's `board_id` is just a
 * field on a row.
 */
function eventFrom(value: unknown, frameBoardId: string | null): RealtimeEvent | null {
  if (!isRecord(value) || frameBoardId === null) {
    return null;
  }

  const id = str(value, "id");
  const type = value.type;
  const actorId = str(value, "actor_id");
  const occurredAt = str(value, "occurred_at");

  if (id === null || typeof type !== "string" || actorId === null || occurredAt === null) {
    return null;
  }

  const common = { id, boardId: frameBoardId, actorId, occurredAt };
  const payload = value.payload;

  switch (type) {
    case "card.created":
    case "card.updated": {
      const card = parseCard(unwrap(payload, "card"));

      return card === null ? null : { ...common, type, card };
    }

    case "card.moved": {
      const card = parseCard(unwrap(payload, "card"));
      const fromColumnId = str(payload, "from_column_id");

      if (card === null || fromColumnId === null) {
        return null;
      }

      return {
        ...common,
        type: "card.moved",
        card,
        fromColumnId,
        afterCardId: anchor(payload, "after_card_id"),
      };
    }

    case "card.deleted": {
      const cardId = str(payload, "card_id");
      const columnId = str(payload, "column_id");

      if (cardId === null || columnId === null) {
        return null;
      }

      return { ...common, type: "card.deleted", cardId, columnId };
    }

    case "column.created":
    case "column.updated": {
      const column = parseColumn(unwrap(payload, "column"));

      return column === null ? null : { ...common, type, column };
    }

    case "column.moved": {
      const column = parseColumn(unwrap(payload, "column"));

      if (column === null) {
        return null;
      }

      return {
        ...common,
        type: "column.moved",
        column,
        afterColumnId: anchor(payload, "after_column_id"),
      };
    }

    case "column.deleted": {
      const columnId = str(payload, "column_id");

      return columnId === null ? null : { ...common, type: "column.deleted", columnId };
    }

    case "board.updated": {
      // The payload carries the whole board, but the only part this screen can
      // act on is "something about the board changed" — the name is rendered by
      // the Server Component above `BoardView`, so a re-read is what shows it.
      const board = unwrap(payload, "board");

      return str(board, "id") === null
        ? null
        : { ...common, type: "board.updated", boardId: frameBoardId };
    }

    case "board.deleted": {
      const boardId = str(payload, "board_id");

      return boardId === null ? null : { ...common, type: "board.deleted", boardId };
    }

    default:
      return null;
  }
}

/** Frame types this client sends (`clientFrame` in `event.go`). */
export type ClientFrame =
  | { type: "subscribe"; board_id: string }
  | { type: "unsubscribe"; board_id: string }
  | { type: "ping" };

/** The subprotocol the server selects, echoed back on a successful upgrade. */
export const SUBPROTOCOL = "collabboard.v1";

/**
 * The prefix that carries a bearer token in a subprotocol offer.
 *
 * It exists because a browser cannot set a header on a WebSocket handshake.
 * **This app does not use it from a browser** — see
 * `docs/adr/0010-realtime-browser-credential.md` and
 * `app/api/realtime/boards/[boardId]/route.ts`: the handshake happens on the
 * Next server, where the token already is, and the browser is given a
 * same-origin event stream instead. The constant lives here because the
 * protocol is described in one file.
 */
export const BEARER_SUBPROTOCOL_PREFIX = "bearer.collabboard.v1.";
