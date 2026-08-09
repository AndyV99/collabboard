/**
 * The optimistic reducer, as arithmetic.
 *
 * `lib/board/mutations.ts` is pure, so what an edit does to the board on screen
 * is testable without a component, a network stub or a transition. The
 * behaviour that matters most — what happens when the server refuses — is not
 * in here, because it is not in that file either: React discards an optimistic
 * value when its transition ends, so the rollback is structural. It is
 * exercised against the real components in `board-editing.test.tsx`.
 */

import { describe, expect, it } from "vitest";

import type { Card, Column } from "@/lib/api/types";
import {
  applyBoardChange,
  isPendingId,
  moveAnchor,
  pendingId,
} from "@/lib/board/mutations";
import { groupCardsIntoColumns } from "@/lib/board/snapshot";

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

function card(id: string, columnId: string, title: string, description = ""): Card {
  return {
    id,
    boardId: BOARD,
    columnId,
    title,
    description,
    createdAt: "2026-08-02T09:00:00Z",
    updatedAt: "2026-08-02T09:00:00Z",
  };
}

const COLUMNS = [column("c-1", "To do"), column("c-2", "Doing"), column("c-3", "Done")];

// Deliberately not in creation or alphabetical order: the board's order is the
// server's, and a reducer that quietly sorted would be caught by this fixture.
const CARDS = [
  card("card-3", "c-2", "Zebra"),
  card("card-2", "c-2", "Kilo"),
  card("card-1", "c-2", "Alpha"),
  card("card-4", "c-1", "Mike"),
];

const SNAPSHOT = groupCardsIntoColumns(COLUMNS, CARDS);

/** Column names in order, which is what a reorder is judged on. */
function names(snapshot = SNAPSHOT) {
  return snapshot.columns.map((entry) => entry.column.name);
}

/** Card titles in one column, in order. */
function titles(snapshot: typeof SNAPSHOT, columnId: string) {
  return (
    snapshot.columns
      .find((entry) => entry.column.id === columnId)
      ?.cards.map((entry) => entry.title) ?? []
  );
}

describe("pending ids", () => {
  it("marks an invented id as one, and a server id as not", () => {
    expect(isPendingId(pendingId())).toBe(true);
    expect(isPendingId("2f1c1c4a-0f5e-4f2a-9d0e-6a0d0f0b1c2d")).toBe(false);
  });

  it("does not collide with itself", () => {
    expect(pendingId()).not.toBe(pendingId());
  });
});

describe("cards", () => {
  it("appends a created card to the bottom of its own column", () => {
    const next = applyBoardChange(SNAPSHOT, {
      kind: "card.created",
      columnId: "c-2",
      card: card("new", "c-2", "Fresh"),
    });

    // Bottom, because CreateCard allocates a position past the current maximum.
    expect(titles(next, "c-2")).toEqual(["Zebra", "Kilo", "Alpha", "Fresh"]);
    expect(titles(next, "c-1")).toEqual(["Mike"]);
  });

  it("updates only the named card, and only the named fields", () => {
    const next = applyBoardChange(SNAPSHOT, {
      kind: "card.updated",
      cardId: "card-2",
      title: "Kilo renamed",
    });

    const edited = next.columns[1].cards[1];

    expect(edited.title).toBe("Kilo renamed");
    // Omitted means "leave it alone", exactly as the API's nullable pointer does.
    expect(edited.description).toBe("");
    expect(titles(next, "c-2")).toEqual(["Zebra", "Kilo renamed", "Alpha"]);
  });

  it("clears a description when asked to, rather than treating empty as absent", () => {
    const withText = groupCardsIntoColumns(COLUMNS, [
      card("card-9", "c-1", "Has text", "Something"),
    ]);

    const next = applyBoardChange(withText, {
      kind: "card.updated",
      cardId: "card-9",
      description: "",
    });

    // `optionalText(..., allowEmpty: true)` on the API means "" is a value.
    expect(next.columns[0].cards[0].description).toBe("");
  });

  it("removes a deleted card and leaves the rest in order", () => {
    const next = applyBoardChange(SNAPSHOT, { kind: "card.deleted", cardId: "card-2" });

    expect(titles(next, "c-2")).toEqual(["Zebra", "Alpha"]);
  });

  it("ignores an edit to a card that is no longer there", () => {
    // The replay case: another change already removed it from the base.
    const next = applyBoardChange(SNAPSHOT, {
      kind: "card.updated",
      cardId: "gone",
      title: "x",
    });

    expect(next.columns.map((entry) => entry.cards.length)).toEqual([1, 3, 0]);
  });

  it("does not mutate the snapshot it was given", () => {
    applyBoardChange(SNAPSHOT, { kind: "card.deleted", cardId: "card-2" });

    // React replays pending changes over the latest base on every render, so an
    // in-place edit would corrupt the value being replayed against.
    expect(titles(SNAPSHOT, "c-2")).toEqual(["Zebra", "Kilo", "Alpha"]);
  });
});

describe("columns", () => {
  it("appends a created column to the right-hand end", () => {
    const next = applyBoardChange(SNAPSHOT, {
      kind: "column.created",
      column: column("c-4", "Blocked"),
    });

    expect(names(next)).toEqual(["To do", "Doing", "Done", "Blocked"]);
    expect(next.columns[3].cards).toEqual([]);
  });

  it("renames one column and touches nothing else", () => {
    const next = applyBoardChange(SNAPSHOT, {
      kind: "column.renamed",
      columnId: "c-2",
      name: "In progress",
    });

    expect(names(next)).toEqual(["To do", "In progress", "Done"]);
    expect(titles(next, "c-2")).toEqual(["Zebra", "Kilo", "Alpha"]);
  });

  it("takes a deleted column's cards with it", () => {
    const next = applyBoardChange(SNAPSHOT, { kind: "column.deleted", columnId: "c-2" });

    // ON DELETE CASCADE on the composite foreign key: the server never passes
    // through a state where the column is gone and its cards are not.
    expect(names(next)).toEqual(["To do", "Done"]);
    expect(next.columns.flatMap((entry) => entry.cards)).toHaveLength(1);
  });
});

describe("reordering a column", () => {
  it("puts it first when the anchor is null", () => {
    const next = applyBoardChange(SNAPSHOT, {
      kind: "column.moved",
      columnId: "c-3",
      afterColumnId: null,
    });

    expect(names(next)).toEqual(["Done", "To do", "Doing"]);
  });

  it("puts it immediately after the column named", () => {
    const next = applyBoardChange(SNAPSHOT, {
      kind: "column.moved",
      columnId: "c-1",
      afterColumnId: "c-2",
    });

    expect(names(next)).toEqual(["Doing", "To do", "Done"]);
  });

  it("carries the column's cards along with it", () => {
    const next = applyBoardChange(SNAPSHOT, {
      kind: "column.moved",
      columnId: "c-2",
      afterColumnId: null,
    });

    expect(names(next)).toEqual(["Doing", "To do", "Done"]);
    expect(titles(next, "c-2")).toEqual(["Zebra", "Kilo", "Alpha"]);
  });

  it("leaves the board alone when the anchor names nothing", () => {
    // The stale-anchor case, which the API answers with a 409. Doing nothing is
    // right: the board that does not move is the one still correct in a moment.
    const next = applyBoardChange(SNAPSHOT, {
      kind: "column.moved",
      columnId: "c-1",
      afterColumnId: "not-on-this-board",
    });

    expect(names(next)).toEqual(["To do", "Doing", "Done"]);
  });

  it("resolves the anchor after lifting the column out, so 'right by one' works", () => {
    // The subtle one. Moving c-1 right means anchoring it to c-2 — which is
    // only the correct neighbour once c-1 itself is out of the list, exactly as
    // the API's `AND id <> @column_id` makes it.
    const anchor = moveAnchor(SNAPSHOT.columns, "c-1", "right");

    expect(anchor).toEqual({ afterColumnId: "c-2" });
    expect(
      names(applyBoardChange(SNAPSHOT, {
        kind: "column.moved",
        columnId: "c-1",
        afterColumnId: "c-2",
      })),
    ).toEqual(["Doing", "To do", "Done"]);
  });
});

describe("moveAnchor", () => {
  it("has no move for the first column going left, or the last going right", () => {
    expect(moveAnchor(SNAPSHOT.columns, "c-1", "left")).toBeUndefined();
    expect(moveAnchor(SNAPSHOT.columns, "c-3", "right")).toBeUndefined();
  });

  it("moves the second column to the front with a null anchor", () => {
    // There is no column before the first one to name, so "first" is null —
    // the one position no sibling's id can express.
    expect(moveAnchor(SNAPSHOT.columns, "c-2", "left")).toEqual({ afterColumnId: null });
  });

  it("anchors a leftward move two places back", () => {
    expect(moveAnchor(SNAPSHOT.columns, "c-3", "left")).toEqual({ afterColumnId: "c-1" });
  });

  it("anchors a rightward move to the next column", () => {
    expect(moveAnchor(SNAPSHOT.columns, "c-2", "right")).toEqual({ afterColumnId: "c-3" });
  });

  it("has no move for a column that is not on the board", () => {
    expect(moveAnchor(SNAPSHOT.columns, "nope", "left")).toBeUndefined();
  });

  it("round-trips: left then right returns the board to where it started", () => {
    const left = applyBoardChange(SNAPSHOT, {
      kind: "column.moved",
      columnId: "c-3",
      afterColumnId: moveAnchor(SNAPSHOT.columns, "c-3", "left")!.afterColumnId,
    });

    expect(names(left)).toEqual(["To do", "Done", "Doing"]);

    const back = applyBoardChange(left, {
      kind: "column.moved",
      columnId: "c-3",
      afterColumnId: moveAnchor(left.columns, "c-3", "right")!.afterColumnId,
    });

    expect(names(back)).toEqual(["To do", "Doing", "Done"]);
  });
});

describe("the rule this file must not break", () => {
  it("never sorts anything", () => {
    // Cards arrive in an order no comparison would produce. Every operation has
    // to preserve it, because ADR 0004 keeps the rank that defines it off the
    // wire — there is nothing here that could legitimately be sorted by.
    const operations = [
      { kind: "card.created" as const, columnId: "c-1", card: card("n", "c-1", "Aaa") },
      { kind: "card.updated" as const, cardId: "card-3", title: "Aardvark" },
      { kind: "column.renamed" as const, columnId: "c-2", name: "Aaa" },
    ];

    for (const change of operations) {
      expect(titles(applyBoardChange(SNAPSHOT, change), "c-2")[0]).toBe(
        change.kind === "card.updated" ? "Aardvark" : "Zebra",
      );
    }
  });
});
