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
  cardDropTarget,
  cardNudge,
  cardPosition,
  isPendingId,
  isRedundantMove,
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
    assigneeId: null,
    dueAt: null,
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

  it("assigns, reassigns and unassigns, and can tell the three apart", () => {
    /*
     * The one branch in this reducer where `??` would be a bug. `assignee_id`
     * reaches a nullable column and the API distinguishes absent from null, so
     * "nobody is assigned to this any more" has to be expressible — and a
     * reducer that read null as "leave it alone" would draw the *old* assignee
     * until the refresh landed and then visibly drop them.
     */
    const assigned = applyBoardChange(SNAPSHOT, {
      kind: "card.updated",
      cardId: "card-2",
      assigneeId: "user-dana",
    });

    expect(assigned.columns[1].cards[1].assigneeId).toBe("user-dana");

    const reassigned = applyBoardChange(assigned, {
      kind: "card.updated",
      cardId: "card-2",
      assigneeId: "user-sam",
    });

    expect(reassigned.columns[1].cards[1].assigneeId).toBe("user-sam");

    const unassigned = applyBoardChange(reassigned, {
      kind: "card.updated",
      cardId: "card-2",
      assigneeId: null,
    });

    expect(unassigned.columns[1].cards[1].assigneeId).toBeNull();
  });

  it("leaves the assignee alone when the change does not mention one", () => {
    const assigned = applyBoardChange(SNAPSHOT, {
      kind: "card.updated",
      cardId: "card-2",
      assigneeId: "user-dana",
      dueAt: "2026-08-31T17:00:00Z",
    });

    // A rename must not unassign the card, which is exactly what an
    // `assigneeId: undefined` treated as null would do.
    const renamed = applyBoardChange(assigned, {
      kind: "card.updated",
      cardId: "card-2",
      title: "Kilo renamed",
    });

    expect(renamed.columns[1].cards[1]).toMatchObject({
      title: "Kilo renamed",
      assigneeId: "user-dana",
      dueAt: "2026-08-31T17:00:00Z",
    });
  });

  it("sets and clears a due date", () => {
    const dated = applyBoardChange(SNAPSHOT, {
      kind: "card.updated",
      cardId: "card-2",
      dueAt: "2026-08-31T17:00:00Z",
    });

    expect(dated.columns[1].cards[1].dueAt).toBe("2026-08-31T17:00:00Z");

    const cleared = applyBoardChange(dated, {
      kind: "card.updated",
      cardId: "card-2",
      dueAt: null,
    });

    expect(cleared.columns[1].cards[1].dueAt).toBeNull();
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

describe("moving a card", () => {
  /** Applies a move and reports the column it landed in, top to bottom. */
  function afterMove(
    cardId: string,
    columnId: string,
    afterCardId: string | null,
    snapshot = SNAPSHOT,
  ) {
    return applyBoardChange(snapshot, {
      kind: "card.moved",
      cardId,
      columnId,
      afterCardId,
    });
  }

  it("re-inserts the card after the anchor, within its own column", () => {
    // Zebra, Kilo, Alpha → Kilo, Alpha, Zebra.
    expect(titles(afterMove("card-3", "c-2", "card-1"), "c-2")).toEqual([
      "Kilo",
      "Alpha",
      "Zebra",
    ]);
  });

  it("reads a null anchor as first, the way the API does", () => {
    expect(titles(afterMove("card-1", "c-2", null), "c-2")).toEqual([
      "Alpha",
      "Zebra",
      "Kilo",
    ]);
  });

  it("resolves the anchor after lifting the card out, so a one-place nudge works", () => {
    // Moving Zebra after Kilo means one place down, not two. The anchor is
    // resolved against the list with Zebra already removed — `MoveCard`'s
    // anchor CTE excludes the moving row for exactly this reason.
    expect(titles(afterMove("card-3", "c-2", "card-2"), "c-2")).toEqual([
      "Kilo",
      "Zebra",
      "Alpha",
    ]);
  });

  it("takes the card out of the column it was in when it crosses", () => {
    const moved = afterMove("card-3", "c-1", "card-4");

    expect(titles(moved, "c-1")).toEqual(["Mike", "Zebra"]);
    expect(titles(moved, "c-2")).toEqual(["Kilo", "Alpha"]);
  });

  it("moves a card into an empty column", () => {
    expect(titles(afterMove("card-3", "c-3", null), "c-3")).toEqual(["Zebra"]);
  });

  it("rewrites the card's own columnId, not just which bucket it sits in", () => {
    // The detail panel names the column from `card.columnId`. A card drawn in
    // Done that still says it is in Doing is a screen contradicting itself.
    const moved = afterMove("card-3", "c-3", null);

    expect(moved.columns[2].cards[0].columnId).toBe("c-3");
  });

  it("does nothing when the anchor is not in the target column", () => {
    // The stale drag. The API answers 409 and this answers by leaving the board
    // alone: the optimistic move that declines to guess is the one that still
    // agrees with the server a moment later.
    expect(titles(afterMove("card-3", "c-2", "card-4"), "c-2")).toEqual([
      "Zebra",
      "Kilo",
      "Alpha",
    ]);
  });

  it("does nothing when the target column is not on the board", () => {
    expect(titles(afterMove("card-3", "c-gone", null), "c-2")).toEqual([
      "Zebra",
      "Kilo",
      "Alpha",
    ]);
  });

  it("does nothing when the card has been deleted underneath the move", () => {
    expect(titles(afterMove("card-gone", "c-1", null), "c-1")).toEqual(["Mike"]);
  });

  it("does not mutate the snapshot it was given", () => {
    afterMove("card-3", "c-1", "card-4");

    expect(titles(SNAPSHOT, "c-2")).toEqual(["Zebra", "Kilo", "Alpha"]);
    expect(titles(SNAPSHOT, "c-1")).toEqual(["Mike"]);
  });
});

describe("cardPosition", () => {
  it("reports the column and a 1-based place in it", () => {
    expect(cardPosition(SNAPSHOT, "card-2")).toEqual({
      columnName: "Doing",
      index: 2,
      total: 3,
    });
  });

  it("is undefined for a card that is not on the board", () => {
    expect(cardPosition(SNAPSHOT, "card-gone")).toBeUndefined();
  });
});

describe("cardNudge — the keyboard reaching every placement", () => {
  it("moves down by naming the card below as the anchor", () => {
    expect(cardNudge(SNAPSHOT, "card-3", "down")).toEqual({
      cardId: "card-3",
      columnId: "c-2",
      afterCardId: "card-2",
    });
  });

  it("moves up by naming the card two places above, or null at the top", () => {
    // Alpha is third. To land second it goes after Zebra, which is first.
    expect(cardNudge(SNAPSHOT, "card-1", "up")).toEqual({
      cardId: "card-1",
      columnId: "c-2",
      afterCardId: "card-3",
    });

    // Kilo is second. To land first there is no anchor to name.
    expect(cardNudge(SNAPSHOT, "card-2", "up")).toEqual({
      cardId: "card-2",
      columnId: "c-2",
      afterCardId: null,
    });
  });

  it("refuses to move past either end of a column", () => {
    expect(cardNudge(SNAPSHOT, "card-3", "up")).toBeUndefined();
    expect(cardNudge(SNAPSHOT, "card-1", "down")).toBeUndefined();
  });

  it("crosses to the next column keeping the card's row", () => {
    // Kilo is second in Doing; To do has one card, so second is after Mike.
    expect(cardNudge(SNAPSHOT, "card-2", "left")).toEqual({
      cardId: "card-2",
      columnId: "c-1",
      afterCardId: "card-4",
    });
  });

  it("clamps to the end of a shorter column rather than inventing a row", () => {
    // Alpha is third in Doing and To do has one card: the third row does not
    // exist there, so the card goes after the last one that does.
    expect(cardNudge(SNAPSHOT, "card-1", "left")).toEqual({
      cardId: "card-1",
      columnId: "c-1",
      afterCardId: "card-4",
    });
  });

  it("crosses into an empty column with a null anchor", () => {
    expect(cardNudge(SNAPSHOT, "card-2", "right")).toEqual({
      cardId: "card-2",
      columnId: "c-3",
      afterCardId: null,
    });
  });

  it("refuses to move past either end of the board", () => {
    expect(cardNudge(SNAPSHOT, "card-4", "left")).toBeUndefined();
    expect(cardNudge(applyBoardChange(SNAPSHOT, {
      kind: "card.moved",
      cardId: "card-3",
      columnId: "c-3",
      afterCardId: null,
    }), "card-3", "right")).toBeUndefined();
  });

  it("reaches any position in any column by repetition, which is the whole claim", () => {
    // Alpha, bottom of Doing, to the top of To do. Left clamps it to the end of
    // the shorter column, then one Up puts it first — and the board is the same
    // one a drag there would have produced.
    let board = SNAPSHOT;

    for (const direction of ["left", "up"] as const) {
      const move = cardNudge(board, "card-1", direction);

      expect(move).toBeDefined();
      board = applyBoardChange(board, { kind: "card.moved", ...move! });
    }

    expect(titles(board, "c-1")).toEqual(["Alpha", "Mike"]);
    expect(titles(board, "c-2")).toEqual(["Zebra", "Kilo"]);
  });

  it("skips a card the server has not acknowledged when picking an anchor", () => {
    // A `pending:` id is not a uuid and the API answers 400 to one. Pending
    // cards are always at the bottom, so the nearest real anchor is the last
    // settled card — which is also the truest thing that can be said.
    const invented = pendingId();
    const withPending = groupCardsIntoColumns(COLUMNS, [
      ...CARDS,
      card(invented, "c-1", "Not yet"),
    ]);

    // Alpha is third in Doing. To do is now [Mike, Not yet], so the third row
    // clamps to the second — which is the unacknowledged card. The anchor walks
    // back to Mike rather than naming an id the API would answer 400 to.
    expect(cardNudge(withPending, "card-1", "left")?.afterCardId).toBe("card-4");
  });
});

describe("a column the server has not acknowledged is not a destination", () => {
  // The mirror of the pending-*card* rule, and the more dangerous half: an
  // anchor that is a `pending:` id can be walked back from, but a `column_id`
  // that is one has nowhere to fall back to and goes straight onto the wire as
  // a 400. The board draws a pending column with a live drop zone, so this is
  // reachable by pressing ArrowRight — not a theoretical hole.
  const invented = pendingId();

  it("refuses to nudge a card into one", () => {
    const inLast = applyBoardChange(SNAPSHOT, {
      kind: "card.moved",
      cardId: "card-1",
      columnId: "c-3",
      afterCardId: null,
    });
    const board = applyBoardChange(inLast, {
      kind: "column.created",
      column: column(invented, "Being created"),
    });

    expect(cardNudge(board, "card-1", "right")).toBeUndefined();
  });

  it("refuses to drop a card into one", () => {
    const board = applyBoardChange(SNAPSHOT, {
      kind: "column.created",
      column: column(invented, "Being created"),
    });

    expect(cardDropTarget(board, "card-1", { column: invented }, false)).toBeUndefined();
  });

  it("steps over one to reach the settled column beyond it", () => {
    // Refusing outright would be the easy fix and the wrong one: it would make
    // a real column unreachable for as long as someone else's new one sat
    // between them.
    const board = groupCardsIntoColumns(
      [column("c-1", "One"), column(invented, "Being created"), column("c-2", "Two")],
      [card("only", "c-1", "Only")],
    );

    expect(cardNudge(board, "only", "right")).toEqual({
      cardId: "only",
      columnId: "c-2",
      afterCardId: null,
    });
  });
});

describe("cardDropTarget — the pointer's landing place", () => {
  it("lands after the card it was dragged down onto", () => {
    // Zebra is first, dragged onto Alpha which is third: the sortable strategy
    // has opened the gap below Alpha, so the anchor is Alpha.
    expect(cardDropTarget(SNAPSHOT, "card-3", { card: "card-1" }, false)).toEqual({
      cardId: "card-3",
      columnId: "c-2",
      afterCardId: "card-1",
    });
  });

  it("lands before the card it was dragged up onto", () => {
    // Alpha is third, dragged onto Zebra which is first: the gap is above
    // Zebra, so there is no anchor to name.
    expect(cardDropTarget(SNAPSHOT, "card-1", { card: "card-3" }, true)).toEqual({
      cardId: "card-1",
      columnId: "c-2",
      afterCardId: null,
    });
  });

  it("ignores which side of the card the pointer is on, within a column", () => {
    // Deliberate: the library is already drawing the gap from the indices, and
    // committing anything else puts the card one place from where the user was
    // looking. Both calls differ only in `after` and must agree.
    expect(cardDropTarget(SNAPSHOT, "card-3", { card: "card-1" }, true)).toEqual(
      cardDropTarget(SNAPSHOT, "card-3", { card: "card-1" }, false),
    );
  });

  it("uses the pointer's side when the card is entering another column", () => {
    expect(
      cardDropTarget(SNAPSHOT, "card-3", { card: "card-4" }, false)?.afterCardId,
    ).toBeNull();

    expect(
      cardDropTarget(SNAPSHOT, "card-3", { card: "card-4" }, true)?.afterCardId,
    ).toBe("card-4");
  });

  it("appends when the drop is on the column rather than on a card", () => {
    // The empty space under the last card, which is the only way to reach the
    // bottom of a full column with a pointer.
    expect(cardDropTarget(SNAPSHOT, "card-3", { column: "c-2" }, false)).toEqual({
      cardId: "card-3",
      columnId: "c-2",
      afterCardId: "card-1",
    });
  });

  it("drops into an empty column with a null anchor", () => {
    expect(cardDropTarget(SNAPSHOT, "card-3", { column: "c-3" }, false)).toEqual({
      cardId: "card-3",
      columnId: "c-3",
      afterCardId: null,
    });
  });

  it("has no answer for a card dropped on itself", () => {
    expect(cardDropTarget(SNAPSHOT, "card-3", { card: "card-3" }, false)).toBeUndefined();
  });

  it("has no answer for a card or column that is not on the board", () => {
    expect(cardDropTarget(SNAPSHOT, "gone", { card: "card-1" }, false)).toBeUndefined();
    expect(cardDropTarget(SNAPSHOT, "card-3", { card: "gone" }, false)).toBeUndefined();
    expect(cardDropTarget(SNAPSHOT, "card-3", { column: "gone" }, false)).toBeUndefined();
  });
});

describe("isRedundantMove", () => {
  it("is true for the placement a card already has", () => {
    // Kilo is second in Doing, which is "after Zebra".
    expect(
      isRedundantMove(SNAPSHOT, {
        cardId: "card-2",
        columnId: "c-2",
        afterCardId: "card-3",
      }),
    ).toBe(true);
  });

  it("is true for the first card being asked to go first", () => {
    expect(
      isRedundantMove(SNAPSHOT, {
        cardId: "card-3",
        columnId: "c-2",
        afterCardId: null,
      }),
    ).toBe(true);
  });

  it("is true for a card anchored on itself, which the API answers with 409", () => {
    // `MoveCard`'s anchor CTE excludes the moving row, so this matches nothing
    // and is refused rather than being the no-op it looks like. It is what
    // "drop the card back where it came from" computes to, so it is not rare.
    expect(
      isRedundantMove(SNAPSHOT, {
        cardId: "card-2",
        columnId: "c-2",
        afterCardId: "card-2",
      }),
    ).toBe(true);
  });

  it("is false for a real move, including one that only changes column", () => {
    expect(
      isRedundantMove(SNAPSHOT, {
        cardId: "card-2",
        columnId: "c-2",
        afterCardId: null,
      }),
    ).toBe(false);

    expect(
      isRedundantMove(SNAPSHOT, {
        cardId: "card-4",
        columnId: "c-3",
        afterCardId: null,
      }),
    ).toBe(false);
  });
});

/**
 * The two properties the hand-picked cases above only sample.
 *
 * `cardDropTarget`'s within-column rule is the subtlest thing in this file and
 * the likeliest place for an off-by-one to hide, because it has to agree with a
 * gap something else is drawing: `@dnd-kit`'s sorting strategy opens the space
 * using `arrayMove` semantics, and committing anything else lands the card one
 * place from where the user was looking. Rather than trust three examples, this
 * asserts the agreement for *every* pair of positions in a six-card column —
 * and for both values of `after`, since within a column the pointer's side of
 * the card must not matter.
 */
describe("within a column, the commit matches the gap the library drew", () => {
  const SIZE = 6;
  const ids = Array.from({ length: SIZE }, (_, index) => `k${index}`);
  const board = groupCardsIntoColumns(
    [column("c-1", "One"), column("c-2", "Two")],
    ids.map((id) => card(id, "c-1", id)),
  );

  /** What `@dnd-kit`'s sorting strategy shows while `from` is held over `to`. */
  function arrayMove(list: readonly string[], from: number, to: number): string[] {
    const copy = [...list];

    copy.splice(to, 0, ...copy.splice(from, 1));

    return copy;
  }

  for (let from = 0; from < SIZE; from += 1) {
    for (let to = 0; to < SIZE; to += 1) {
      if (from === to) {
        continue;
      }

      it(`${from} → ${to}`, () => {
        for (const after of [false, true]) {
          const move = cardDropTarget(board, ids[from], { card: ids[to] }, after);

          expect(move, `no move for ${from} → ${to}`).toBeDefined();

          const next = applyBoardChange(board, { kind: "card.moved", ...move! });

          expect(
            next.columns[0].cards.map((entry) => entry.id),
            `after=${after}, ${from} → ${to}`,
          ).toEqual(arrayMove(ids, from, to));
        }
      });
    }
  }
});

/**
 * Every keyboard nudge is undone by its opposite.
 *
 * A round trip is the cheapest statement of "these two anchors are each other's
 * inverse", and it catches the asymmetry the up case invites — up names the card
 * *two* places back while down names the one immediately after, so getting
 * either wrong by one leaves the card somewhere the other direction cannot
 * return it from. Run from every position in three columns of different
 * lengths, including the ends, where the answer is that there is no move.
 */
describe("nudging a card and nudging it back is the identity", () => {
  const board = groupCardsIntoColumns(
    [column("c-1", "One"), column("c-2", "Two"), column("c-3", "Three")],
    [
      card("a", "c-1", "a"),
      card("b", "c-1", "b"),
      card("c", "c-1", "c"),
      card("d", "c-2", "d"),
      card("e", "c-2", "e"),
      card("f", "c-3", "f"),
    ],
  );

  const shape = (snapshot: typeof board) =>
    snapshot.columns
      .map((entry) => `${entry.column.id}:${entry.cards.map((c) => c.id).join(",")}`)
      .join(" | ");

  const pairs = [
    ["up", "down"],
    ["down", "up"],
    ["left", "right"],
    ["right", "left"],
  ] as const;

  for (const id of ["a", "b", "c", "d", "e", "f"]) {
    for (const [there, back] of pairs) {
      it(`${id}: ${there} then ${back}`, () => {
        const first = cardNudge(board, id, there);

        // At an edge there is no move, which is itself the right answer.
        if (first === undefined) {
          return;
        }

        const moved = applyBoardChange(board, { kind: "card.moved", ...first });

        // Guards the whole test: a `cardNudge` that returned a no-op move would
        // otherwise satisfy the round trip without moving anything.
        expect(shape(moved)).not.toBe(shape(board));

        const second = cardNudge(moved, id, back);

        expect(second, `${id} moved ${there} but cannot come ${back}`).toBeDefined();

        expect(
          shape(applyBoardChange(moved, { kind: "card.moved", ...second! })),
        ).toBe(shape(board));
      });
    }
  }
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
