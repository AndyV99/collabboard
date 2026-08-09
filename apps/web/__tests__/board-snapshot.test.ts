/**
 * The one rule the board view has: the API's order is the order.
 *
 * ADR 0004 keeps a card's rank off the wire, so there is nothing in a payload
 * to sort by and the array's sequence is the whole answer. These tests are
 * written to fail if anybody ever adds a comparison — the fixtures are
 * deliberately in an order that no plausible sort key would reproduce, so a
 * `.sort()` on `title`, `createdAt` or `id` breaks them rather than passing by
 * luck.
 */

import { describe, expect, it } from "vitest";

import type { Card, Column } from "@/lib/api/types";
import {
  columnNameOf,
  countCards,
  findCard,
  groupCardsIntoColumns,
} from "@/lib/board/snapshot";

const BOARD = "b-1";

function column(id: string, name: string): Column {
  return {
    id,
    boardId: BOARD,
    name,
    createdAt: "2026-08-01T09:00:00Z",
    updatedAt: "2026-08-01T09:00:00Z",
  };
}

function card(id: string, columnId: string, title: string, createdAt: string): Card {
  return {
    id,
    boardId: BOARD,
    columnId,
    title,
    description: "",
    createdAt,
    updatedAt: createdAt,
  };
}

/**
 * A board whose cards have been moved, which is the only board that proves
 * anything about ordering.
 *
 * In "Doing", the card created *last* ("Zebra", 11:00) is first and the one
 * created first is last. Alphabetically the titles run Zebra, Kilo, Alpha —
 * reverse order. So the expected sequence matches neither `createdAt` ascending
 * nor descending, nor `title`, nor the card ids.
 */
const COLUMNS = [column("c-todo", "To do"), column("c-doing", "Doing")];

const CARDS = [
  // The API orders by column_id first, so the array is grouped by column but
  // the *columns* are not in display order — c-doing sorts before c-todo.
  card("card-3", "c-doing", "Zebra", "2026-08-02T11:00:00Z"),
  card("card-2", "c-doing", "Kilo", "2026-08-02T10:00:00Z"),
  card("card-1", "c-doing", "Alpha", "2026-08-02T09:00:00Z"),
  card("card-4", "c-todo", "Mike", "2026-08-02T12:00:00Z"),
];

describe("groupCardsIntoColumns", () => {
  it("keeps the columns in the order the API returned them", () => {
    const snapshot = groupCardsIntoColumns(COLUMNS, CARDS);

    expect(snapshot.columns.map((entry) => entry.column.name)).toEqual([
      "To do",
      "Doing",
    ]);
  });

  it("keeps each column's cards in the order the API returned them", () => {
    const snapshot = groupCardsIntoColumns(COLUMNS, CARDS);

    // Not creation order, not reverse creation order, not alphabetical: the
    // order a move put them in.
    expect(snapshot.columns[1].cards.map((c) => c.title)).toEqual([
      "Zebra",
      "Kilo",
      "Alpha",
    ]);
  });

  it("does not reorder cards across columns to match the column order", () => {
    // The cards response is grouped by column_id (a uuid), which is unrelated
    // to the columns' display order. Grouping must follow column_id, not the
    // position of the card in the flat list.
    const snapshot = groupCardsIntoColumns(COLUMNS, CARDS);

    expect(snapshot.columns[0].cards.map((c) => c.id)).toEqual(["card-4"]);
    expect(snapshot.columns[1].cards.map((c) => c.id)).toEqual([
      "card-3",
      "card-2",
      "card-1",
    ]);
  });

  it("gives a column with no cards an empty list rather than omitting it", () => {
    const snapshot = groupCardsIntoColumns(COLUMNS, [CARDS[0]]);

    expect(snapshot.columns).toHaveLength(2);
    expect(snapshot.columns[0].cards).toEqual([]);
  });

  it("renders a board with no columns and no cards as an empty board", () => {
    const snapshot = groupCardsIntoColumns([], []);

    expect(snapshot.columns).toEqual([]);
    expect(snapshot.unplaced).toEqual([]);
    expect(countCards(snapshot)).toBe(0);
  });

  it("reports a card whose column is not in the columns response", () => {
    // Two requests are not one snapshot: a column created between them is
    // absent from the first and has cards in the second.
    const orphan = card("card-9", "c-new", "Made moments ago", "2026-08-03T08:00:00Z");
    const snapshot = groupCardsIntoColumns(COLUMNS, [...CARDS, orphan]);

    expect(snapshot.unplaced.map((c) => c.id)).toEqual(["card-9"]);
    // Counted, so the board's own total never disagrees with what the API sent.
    expect(countCards(snapshot)).toBe(5);
  });

  it("does not mutate the arrays it was given", () => {
    const columns = [...COLUMNS];
    const cards = [...CARDS];

    groupCardsIntoColumns(columns, cards);

    expect(columns).toEqual(COLUMNS);
    expect(cards).toEqual(CARDS);
  });

  it("counts every card on the board", () => {
    expect(countCards(groupCardsIntoColumns(COLUMNS, CARDS))).toBe(4);
  });
});

describe("findCard", () => {
  it("finds a card already in memory rather than needing a request", () => {
    expect(findCard(CARDS, "card-2")?.title).toBe("Kilo");
  });

  it("returns null for an id that is not on this board", () => {
    expect(findCard(CARDS, "card-does-not-exist")).toBeNull();
  });
});

describe("columnNameOf", () => {
  it("resolves the column id a card carries into a name", () => {
    expect(columnNameOf(COLUMNS, CARDS[0])).toBe("Doing");
  });

  it("returns null when the card's column is not on the board", () => {
    const orphan = card("card-9", "c-new", "Made moments ago", "2026-08-03T08:00:00Z");

    expect(columnNameOf(COLUMNS, orphan)).toBeNull();
  });
});
