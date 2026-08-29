/**
 * The wire format, and what an event does to the board.
 *
 * Both halves are pure functions, so this file is a table of inputs and
 * expected outputs with no clock, no socket and no React in it. That is the
 * point: the realtime path has three genuinely hard-to-test things in it
 * (timers, sockets, transitions) and none of them are here, because the parts
 * that decide what an event *means* were deliberately kept away from the parts
 * that decide *when* it arrives.
 *
 * # The idempotence tests are the ones that matter
 *
 * `use-board-live.ts` replays its whole log over every fresh snapshot rather
 * than reasoning about which read included which write. That is only correct if
 * applying an event twice equals applying it once, so the property is asserted
 * directly, per event type, rather than left as a comment.
 */

import { describe, expect, it } from "vitest";

import { applyLiveEvent, applyLiveLog, changeFor } from "@/lib/realtime/apply";
import { type RealtimeEvent, frameFrom, parseFrame } from "@/lib/realtime/protocol";
import { groupCardsIntoColumns } from "@/lib/board/snapshot";
import type { Card, Column } from "@/lib/api/types";

const BOARD = "11111111-1111-4111-8111-111111111111";

function columnBody(id: string, name: string) {
  return {
    id,
    board_id: BOARD,
    name,
    created_at: "2026-08-01T09:00:00Z",
    updated_at: "2026-08-01T09:00:00Z",
  };
}

function cardBody(id: string, columnId: string, title: string, description = "") {
  return {
    id,
    board_id: BOARD,
    column_id: columnId,
    title,
    description,
    assignee_id: null,
    due_at: null,
    created_at: "2026-08-02T09:00:00Z",
    updated_at: "2026-08-02T09:00:00Z",
  };
}

/** A whole `event` frame, as the Go API writes it. */
function eventFrame(type: string, payload: unknown) {
  return {
    type: "event",
    board_id: BOARD,
    event: {
      id: "e-1",
      type,
      actor_id: "22222222-2222-4222-8222-222222222222",
      occurred_at: "2026-08-10T01:19:58.424746926Z",
      payload,
    },
  };
}

/** The parsed event inside a frame, for the apply tests. */
function eventOf(type: string, payload: unknown): RealtimeEvent {
  const frame = frameFrom(eventFrame(type, payload));

  if (frame.kind !== "event") {
    throw new Error(`expected an event frame, got ${frame.kind}`);
  }

  return frame.event;
}

function column(id: string, name: string): Column {
  return {
    id,
    boardId: BOARD,
    name,
    createdAt: "2026-08-01T09:00:00Z",
    updatedAt: "2026-08-01T09:00:00Z",
  };
}

function card(id: string, columnId: string, title: string): Card {
  return {
    id,
    boardId: BOARD,
    columnId,
    title,
    description: "",
    assigneeId: null,
    dueAt: null,
    createdAt: "2026-08-02T09:00:00Z",
    updatedAt: "2026-08-02T09:00:00Z",
  };
}

function board() {
  return groupCardsIntoColumns(
    [column("c-todo", "To do"), column("c-doing", "Doing")],
    [
      card("card-1", "c-doing", "Alpha"),
      card("card-2", "c-doing", "Kilo"),
      card("card-3", "c-doing", "Zebra"),
      card("card-4", "c-todo", "Mike"),
    ],
  );
}

/** A column's card titles, top to bottom. */
function titles(snapshot: ReturnType<typeof board>, columnId: string): string[] {
  return (
    snapshot.columns
      .find((entry) => entry.column.id === columnId)
      ?.cards.map((entry) => entry.title) ?? []
  );
}

describe("parsing a frame", () => {
  it("reads every non-event frame the server sends", () => {
    expect(frameFrom({ type: "subscribed", board_id: BOARD })).toEqual({
      kind: "subscribed",
      boardId: BOARD,
    });

    expect(frameFrom({ type: "unsubscribed", board_id: BOARD })).toEqual({
      kind: "unsubscribed",
      boardId: BOARD,
      reason: null,
    });

    expect(
      frameFrom({ type: "unsubscribed", board_id: BOARD, reason: "forbidden" }),
    ).toEqual({ kind: "unsubscribed", boardId: BOARD, reason: "forbidden" });

    expect(
      frameFrom({
        type: "error",
        board_id: BOARD,
        reason: "too_many_subscriptions",
        message: "unsubscribe from a board before watching another",
      }),
    ).toEqual({
      kind: "error",
      boardId: BOARD,
      reason: "too_many_subscriptions",
      message: "unsubscribe from a board before watching another",
    });

    expect(
      frameFrom({
        type: "shutdown",
        message: "this instance is restarting",
        reconnect_after_ms: 1400,
      }),
    ).toEqual({
      kind: "shutdown",
      message: "this instance is restarting",
      reconnectAfterMs: 1400,
    });

    expect(frameFrom({ type: "pong" })).toEqual({ kind: "pong" });
  });

  it("reads an error frame that carries no board id, which is the invalid-request one", () => {
    // `conn.go` sends this one without a board_id, because the board id is what
    // was wrong. A parser that required the field would drop the only frame
    // explaining why nothing is being delivered.
    expect(
      frameFrom({
        type: "error",
        reason: "invalid_request",
        message: "board_id must be a uuid",
      }),
    ).toEqual({
      kind: "error",
      boardId: null,
      reason: "invalid_request",
      message: "board_id must be a uuid",
    });
  });

  it("calls a frame type it has never heard of unknown rather than failing", () => {
    // The API is allowed to add a frame type without this client being redeployed.
    expect(frameFrom({ type: "something.new", board_id: BOARD })).toEqual({
      kind: "unknown",
    });
  });

  it("returns null only for text that is not JSON", () => {
    expect(parseFrame("not json")).toBeNull();
    expect(parseFrame('{"type":"pong"}')).toEqual({ kind: "pong" });
  });

  it("takes the board id from the frame, not from the payload", () => {
    // The server re-checks the frame's board id against the Redis channel it
    // arrived on and drops a mismatch; the payload's copy is just a column on a
    // row. Trusting the payload would mean trusting whatever was written there.
    const frame = frameFrom({
      type: "event",
      board_id: BOARD,
      event: {
        id: "e-1",
        type: "card.created",
        actor_id: "22222222-2222-4222-8222-222222222222",
        occurred_at: "2026-08-10T01:19:58Z",
        payload: { card: { ...cardBody("card-9", "c-todo", "New"), board_id: "somewhere-else" } },
      },
    });

    expect(frame.kind).toBe("event");
    expect(frame.kind === "event" && frame.event.boardId).toBe(BOARD);
  });
});

describe("parsing each event type", () => {
  it("reads a card.moved with its anchor and the column it left", () => {
    const event = eventOf("card.moved", {
      card: cardBody("card-1", "c-todo", "Alpha"),
      from_column_id: "c-doing",
      after_card_id: "card-4",
    });

    expect(event).toMatchObject({
      type: "card.moved",
      fromColumnId: "c-doing",
      afterCardId: "card-4",
    });
  });

  it("reads an explicitly null anchor as 'first', which is what the server means", () => {
    const event = eventOf("card.moved", {
      card: cardBody("card-1", "c-todo", "Alpha"),
      from_column_id: "c-doing",
      after_card_id: null,
    });

    expect(event.type === "card.moved" && event.afterCardId).toBeNull();
  });

  it("reads the deletes, which carry ids and no body", () => {
    expect(eventOf("card.deleted", { card_id: "card-1", column_id: "c-doing" })).toMatchObject({
      type: "card.deleted",
      cardId: "card-1",
      columnId: "c-doing",
    });

    expect(eventOf("column.deleted", { column_id: "c-doing" })).toMatchObject({
      type: "column.deleted",
      columnId: "c-doing",
    });

    expect(eventOf("board.deleted", { board_id: BOARD })).toMatchObject({
      type: "board.deleted",
      boardId: BOARD,
    });
  });

  it("reads the column events", () => {
    expect(eventOf("column.created", { column: columnBody("c-new", "Blocked") })).toMatchObject({
      type: "column.created",
    });

    expect(eventOf("column.updated", { column: columnBody("c-todo", "Later") })).toMatchObject({
      type: "column.updated",
    });

    expect(
      eventOf("column.moved", {
        column: columnBody("c-doing", "Doing"),
        after_column_id: null,
      }),
    ).toMatchObject({ type: "column.moved", afterColumnId: null });
  });

  it("refuses a payload that is not the shape the REST endpoints return", () => {
    // The whole reason ADR 0005 publishes `cardBody` verbatim is so one decoder
    // serves both. A payload missing a field the parser requires has to fail
    // here rather than reach the board as a card with `undefined` on it.
    const frame = frameFrom(eventFrame("card.created", { card: { id: "card-9" } }));

    expect(frame).toEqual({ kind: "unknown" });
  });
});

describe("applying an event to the board", () => {
  it("carries somebody else's assignment onto this board", () => {
    /*
     * The other half of #157: assigning a card in one browser has to show in
     * another. `patchCardHandler` publishes `newCardBody(card)` — the whole
     * card, not the fields that changed — so this event is a wholesale replace
     * and every field on it is stated, `null` included.
     */
    const assigned = applyLiveEvent(
      board(),
      eventOf("card.updated", {
        card: {
          ...cardBody("card-1", "c-doing", "Alpha"),
          assignee_id: "user-dana",
          due_at: "2026-08-31T17:00:00Z",
        },
      }),
    );

    const card = assigned.columns[1].cards.find((entry) => entry.id === "card-1");

    expect(card).toMatchObject({
      assigneeId: "user-dana",
      dueAt: "2026-08-31T17:00:00Z",
    });

    // And the unassignment, which is the one an `??` in `applyBoardChange`
    // would silently drop — leaving this board showing Dana on a card nobody
    // is on any more.
    const unassigned = applyLiveEvent(
      assigned,
      eventOf("card.updated", { card: cardBody("card-1", "c-doing", "Alpha") }),
    );

    expect(
      unassigned.columns[1].cards.find((entry) => entry.id === "card-1"),
    ).toMatchObject({ assigneeId: null, dueAt: null });
  });

  it("moves a card to the position the anchor names", () => {
    const after = applyLiveEvent(
      board(),
      eventOf("card.moved", {
        card: cardBody("card-3", "c-doing", "Zebra"),
        from_column_id: "c-doing",
        after_card_id: null,
      }),
    );

    expect(titles(after, "c-doing")).toEqual(["Zebra", "Alpha", "Kilo"]);
  });

  it("moves a card across columns and rewrites the column on the card itself", () => {
    const after = applyLiveEvent(
      board(),
      eventOf("card.moved", {
        card: cardBody("card-1", "c-todo", "Alpha"),
        from_column_id: "c-doing",
        after_card_id: "card-4",
      }),
    );

    expect(titles(after, "c-doing")).toEqual(["Kilo", "Zebra"]);
    expect(titles(after, "c-todo")).toEqual(["Mike", "Alpha"]);
  });

  it("adds a created card at the bottom, where the server put it", () => {
    const after = applyLiveEvent(
      board(),
      eventOf("card.created", { card: cardBody("card-9", "c-todo", "New") }),
    );

    expect(titles(after, "c-todo")).toEqual(["Mike", "New"]);
  });

  it("takes a deleted column's cards with it", () => {
    const after = applyLiveEvent(board(), eventOf("column.deleted", { column_id: "c-doing" }));

    expect(after.columns.map((entry) => entry.column.id)).toEqual(["c-todo"]);
  });

  it("does nothing for a board event, which the hook handles instead", () => {
    const before = board();

    expect(applyLiveEvent(before, eventOf("board.deleted", { board_id: BOARD }))).toBe(before);
    expect(changeFor(eventOf("board.updated", { board: { id: BOARD } }))).toBeNull();
  });

  it("leaves the board alone when the event names something it has never seen", () => {
    // Not defensive padding: a client that missed the create is exactly the
    // at-most-once case ADR 0005 describes, and the re-read is what fixes it.
    const before = board();

    expect(
      titles(applyLiveEvent(before, eventOf("card.deleted", { card_id: "ghost", column_id: "c-doing" })), "c-doing"),
    ).toEqual(["Alpha", "Kilo", "Zebra"]);
  });
});

describe("applying the same event twice", () => {
  // The replay contract. `use-board-live.ts` folds its whole log over each new
  // snapshot rather than working out which read included which write, and that
  // is only sound if every one of these is a no-op the second time.
  const cases: Array<[string, RealtimeEvent]> = [
    ["card.created", eventOf("card.created", { card: cardBody("card-9", "c-todo", "New") })],
    [
      "card.updated",
      eventOf("card.updated", { card: cardBody("card-1", "c-doing", "Renamed") }),
    ],
    [
      "card.moved",
      eventOf("card.moved", {
        card: cardBody("card-3", "c-todo", "Zebra"),
        from_column_id: "c-doing",
        after_card_id: "card-4",
      }),
    ],
    ["card.deleted", eventOf("card.deleted", { card_id: "card-2", column_id: "c-doing" })],
    ["column.created", eventOf("column.created", { column: columnBody("c-new", "Blocked") })],
    ["column.updated", eventOf("column.updated", { column: columnBody("c-todo", "Later") })],
    [
      "column.moved",
      eventOf("column.moved", {
        column: columnBody("c-doing", "Doing"),
        after_column_id: null,
      }),
    ],
    ["column.deleted", eventOf("column.deleted", { column_id: "c-todo" })],
  ];

  it.each(cases)("%s applied twice is the same as applied once", (_name, event) => {
    const once = applyLiveEvent(board(), event);
    const twice = applyLiveEvent(once, event);

    expect(twice).toEqual(once);
  });

  it("does not draw a created card twice, which is the branch that needed changing", () => {
    const event = eventOf("card.created", { card: cardBody("card-9", "c-todo", "New") });
    const twice = applyLiveEvent(applyLiveEvent(board(), event), event);

    expect(titles(twice, "c-todo")).toEqual(["Mike", "New"]);
  });

  it("does not draw a created column twice either", () => {
    const event = eventOf("column.created", { column: columnBody("c-new", "Blocked") });
    const twice = applyLiveEvent(applyLiveEvent(board(), event), event);

    expect(twice.columns.map((entry) => entry.column.name)).toEqual([
      "To do",
      "Doing",
      "Blocked",
    ]);
  });

  it("replays a whole log idempotently, in order", () => {
    const log = cases.map(([, event]) => event);

    const once = applyLiveLog(board(), log);
    const twice = applyLiveLog(once, log);

    expect(twice).toEqual(once);
  });

  it("keeps the last of two moves of the same card, in log order", () => {
    // The server's total order per board is the order these arrive in, and
    // replaying must not reorder them. Rule 3: the server decides.
    const toTodo = eventOf("card.moved", {
      card: cardBody("card-1", "c-todo", "Alpha"),
      from_column_id: "c-doing",
      after_card_id: null,
    });

    const backToDoing = eventOf("card.moved", {
      card: cardBody("card-1", "c-doing", "Alpha"),
      from_column_id: "c-todo",
      after_card_id: "card-3",
    });

    const after = applyLiveLog(board(), [toTodo, backToDoing]);

    expect(titles(after, "c-todo")).toEqual(["Mike"]);
    expect(titles(after, "c-doing")).toEqual(["Kilo", "Zebra", "Alpha"]);
  });
});
