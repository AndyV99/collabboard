/**
 * What an edit does to the board *on screen*, while the server decides.
 *
 * Every function here is pure: a {@link BoardSnapshot} and a {@link BoardChange}
 * in, a new snapshot out, no fetching, no clock, no randomness. That is what
 * lets the optimistic behaviour of the whole board be unit tested as arithmetic
 * rather than provoked through seven components and a network stub.
 *
 * # This is a display, never a source of truth
 *
 * `components/boards/board-view.tsx` feeds these through React's
 * [`useOptimistic`](https://react.dev/reference/react/useOptimistic), which
 * holds the result only for as long as the transition that produced it is
 * pending. When the transition ends the value is **discarded** and the board
 * re-renders from the server-rendered prop. So a change applied here survives
 * exactly until the answer arrives, whatever the answer is:
 *
 * - the write succeeded → `router.refresh()` re-renders the page on the server
 *   and the new prop already contains the change;
 * - the write failed → nothing else happens, the prop never changed, and the
 *   board is back where it started.
 *
 * **Rollback is therefore structural rather than a code path.** There is no
 * inverse operation below and there is deliberately nowhere to write one: a
 * hand-written undo is a second implementation of every function here, and the
 * bug it produces is the UI keeping a change the server refused. See
 * `__tests__/board-mutations.test.tsx`, which asserts the refusal case for
 * every one of these operations.
 *
 * # Ordering, and the one rule this file must never break
 *
 * ADR 0004 keeps a card's rank off the wire: it appears in no response and is
 * accepted by no endpoint. `lib/board/snapshot.ts` says the consequence for
 * reads — the array's sequence *is* the order, and nothing sorts it. This file
 * is where that rule is most easily broken, because a reorder is the one edit
 * whose whole content is a position.
 *
 * So {@link applyBoardChange} does not sort, and `column.moved` is expressed
 * the way the API expresses it: **remove the column, re-insert it after a named
 * neighbour**, with a null anchor meaning "first". That mirrors
 * `POST /columns/:id/move`'s `after_column_id` exactly, which means the
 * optimistic board and the server are computing the same thing from the same
 * argument rather than agreeing by coincidence. The server still decides — its
 * answer replaces this one — and a stale anchor it refuses with a 409 takes the
 * optimistic move down with it.
 *
 * # #65: every card move is decided here, not in the gesture
 *
 * Dragging is a gesture; *where the card lands* is arithmetic, and this file is
 * where it happens. {@link cardDropTarget} turns "the pointer is over that card"
 * into a {@link CardMove}, {@link cardNudge} turns "the user pressed Down" into
 * the same type, and {@link applyBoardChange} draws either of them. The drag
 * library and the keyboard handler both reduce to one function call returning
 * one value, so the two input methods cannot drift apart and neither of them
 * needs a board in a browser to be tested.
 *
 * A {@link CardMove} is the request body and nothing more: a target column and
 * an anchor. There is no index in it and no rank, because there is no index or
 * rank in `POST /cards/:id/move` — see ADR 0004's third decision.
 */

import type { Card, Column } from "@/lib/api/types";
import type { BoardSnapshot, ColumnWithCards } from "./snapshot";

/**
 * A single edit, named the way the API names it.
 *
 * One flat union rather than a method per operation, because `useOptimistic`
 * takes one reducer and because a closed union is what makes the exhaustiveness
 * of {@link applyBoardChange} a compile error rather than a missing branch.
 */
export type BoardChange =
  | { kind: "card.created"; columnId: string; card: Card }
  | { kind: "card.updated"; cardId: string; title?: string; description?: string }
  | { kind: "card.deleted"; cardId: string }
  | { kind: "card.moved"; cardId: string; columnId: string; afterCardId: string | null }
  | { kind: "column.created"; column: Column }
  | { kind: "column.renamed"; columnId: string; name: string }
  | { kind: "column.deleted"; columnId: string }
  | { kind: "column.moved"; columnId: string; afterColumnId: string | null };

/**
 * Marks an id the client invented rather than one the server issued.
 *
 * An optimistic card has to be rendered before it has an id, and the obvious
 * shortcut — a `crypto.randomUUID()` that looks exactly like a real one — is a
 * trap: the card tile links to `?card=<id>`, so a user quick enough to click it
 * would open a detail panel for a card that does not exist, and the board could
 * not tell that it had invented the id it was being asked about.
 *
 * A prefix no uuid can have makes the difference checkable. `isPendingId` is
 * what the board uses to render a placeholder as inert text instead of a link.
 */
const PENDING_PREFIX = "pending:";

/** A client-side id for a row the server has not acknowledged yet. */
export function pendingId(): string {
  return `${PENDING_PREFIX}${crypto.randomUUID()}`;
}

/** Whether an id was invented by {@link pendingId} and names nothing yet. */
export function isPendingId(id: string): boolean {
  return id.startsWith(PENDING_PREFIX);
}

/**
 * Applies one change to a snapshot, returning a new one.
 *
 * The reducer behind `useOptimistic`. It never mutates its argument: React
 * replays every pending change over the latest server value on each render, so
 * an in-place edit would corrupt the base it is replayed against.
 *
 * Every branch is a no-op when its subject is missing. That is not defensive
 * padding — it is the correct answer to a real race. Two edits can be in flight
 * at once, and a `card.updated` replayed over a base in which a *different*
 * client has already deleted that card has nothing to update. Dropping it
 * silently leaves the server's version on screen, which is the truth; throwing
 * would take the board down over a card that is already gone.
 */
export function applyBoardChange(
  snapshot: BoardSnapshot,
  change: BoardChange,
): BoardSnapshot {
  switch (change.kind) {
    case "card.created":
      return mapColumns(snapshot, (entry) =>
        entry.column.id === change.columnId
          ? // Appended, because `CreateCard` allocates a position one past the
            // column's current maximum — a new card goes to the bottom. Putting
            // it at the top here would look right until the refresh landed and
            // then visibly jump.
            { ...entry, cards: [...entry.cards, change.card] }
          : entry,
      );

    case "card.updated": {
      const edit = (card: Card): Card => ({
        ...card,
        title: change.title ?? card.title,
        description: change.description ?? card.description,
      });

      return mapColumns(snapshot, (entry) => ({
        ...entry,
        cards: entry.cards.map((card) => (card.id === change.cardId ? edit(card) : card)),
      }));
    }

    case "card.deleted":
      return mapColumns(snapshot, (entry) => ({
        ...entry,
        cards: entry.cards.filter((card) => card.id !== change.cardId),
      }));

    case "card.moved":
      return { ...snapshot, columns: moveCard(snapshot.columns, change) };

    case "column.created":
      // Appended for the same reason a card is: `CreateColumn` allocates one
      // past the current maximum.
      return {
        ...snapshot,
        columns: [...snapshot.columns, { column: change.column, cards: [] }],
      };

    case "column.renamed":
      return mapColumns(snapshot, (entry) =>
        entry.column.id === change.columnId
          ? { ...entry, column: { ...entry.column, name: change.name } }
          : entry,
      );

    case "column.deleted":
      // The cards go with it. `columns` and `cards` share a composite foreign
      // key with ON DELETE CASCADE, so the server deletes them in the same
      // statement — showing the column vanish while its cards lingered would be
      // a state the database never passes through.
      return {
        ...snapshot,
        columns: snapshot.columns.filter((entry) => entry.column.id !== change.columnId),
      };

    case "column.moved":
      return { ...snapshot, columns: moveColumn(snapshot.columns, change) };
  }
}

/** Rebuilds `columns` through a per-entry function, leaving `unplaced` alone. */
function mapColumns(
  snapshot: BoardSnapshot,
  map: (entry: ColumnWithCards) => ColumnWithCards,
): BoardSnapshot {
  return { ...snapshot, columns: snapshot.columns.map(map) };
}

/**
 * Re-inserts one column after a named neighbour.
 *
 * The anchor is resolved **after** the column has been lifted out, which is the
 * detail that makes "move right by one" work. `POST /columns/:id/move` resolves
 * it the same way — the SQL excludes the moving column with `AND id <> @column_id`
 * — so both sides read the same neighbour list.
 *
 * An anchor naming nothing leaves the order untouched rather than guessing.
 * That is the stale-drag case, and the server answers it with a 409, so the
 * board that does nothing is the board that will still be right in a moment.
 */
function moveColumn(
  columns: readonly ColumnWithCards[],
  change: { columnId: string; afterColumnId: string | null },
): ColumnWithCards[] {
  const from = columns.findIndex((entry) => entry.column.id === change.columnId);

  if (from === -1) {
    return [...columns];
  }

  const rest = columns.filter((_, index) => index !== from);
  const moving = columns[from];

  if (change.afterColumnId === null) {
    return [moving, ...rest];
  }

  const anchor = rest.findIndex((entry) => entry.column.id === change.afterColumnId);

  if (anchor === -1) {
    return [...columns];
  }

  return [...rest.slice(0, anchor + 1), moving, ...rest.slice(anchor + 1)];
}

/**
 * Re-inserts one card after a named neighbour, in a possibly different column.
 *
 * The same shape as {@link moveColumn} and for the same reason: the card is
 * lifted out first, then the anchor is resolved against what is left. `MoveCard`
 * in `apps/api/internal/store/query.sql` excludes the moving card from its
 * anchor CTE with `AND id <> @card_id`, so both sides are naming a neighbour in
 * the same list.
 *
 * Three ways this returns the board untouched, each mirroring a refusal the
 * server would produce:
 *
 * - the card is not on the board (it was deleted under us);
 * - the target column is not on the board — the API answers 404 `column not
 *   found`, or 409 if it is a column on another board;
 * - the anchor is not in the target column — the stale drag, which the API
 *   answers with 409 `after_card_id is not a card in that column`.
 *
 * Doing nothing is the right answer in all three: the optimistic board that
 * declines to guess is the one that still agrees with the server a moment later.
 *
 * `columnId` is rewritten on the card itself, not just used to pick a bucket.
 * The detail panel reads `card.columnId` to name the column it is in, so a card
 * dragged to *Done* with a stale `columnId` would sit in Done showing "Doing".
 */
function moveCard(
  columns: readonly ColumnWithCards[],
  change: { cardId: string; columnId: string; afterCardId: string | null },
): ColumnWithCards[] {
  const moving = columns
    .flatMap((entry) => entry.cards)
    .find((card) => card.id === change.cardId);

  if (moving === undefined) {
    return [...columns];
  }

  const target = columns.find((entry) => entry.column.id === change.columnId);

  if (target === undefined) {
    return [...columns];
  }

  // Lifted out of wherever it was, target column included.
  const lifted = columns.map((entry) => ({
    ...entry,
    cards: entry.cards.filter((card) => card.id !== change.cardId),
  }));

  const remaining = lifted.find((entry) => entry.column.id === change.columnId);

  // Unreachable — `target` was found in the same list — but the narrowing is
  // free and an `!` here would be the one place this file lies to the compiler.
  if (remaining === undefined) {
    return [...columns];
  }

  const at =
    change.afterCardId === null
      ? 0
      : remaining.cards.findIndex((card) => card.id === change.afterCardId) + 1;

  // `findIndex` returned -1, so `at` is 0 — indistinguishable from "first",
  // which is why the anchor is checked rather than the index.
  if (change.afterCardId !== null && at === 0) {
    return [...columns];
  }

  const placed: Card = { ...moving, columnId: change.columnId };

  return lifted.map((entry) =>
    entry.column.id === change.columnId
      ? {
          ...entry,
          cards: [
            ...remaining.cards.slice(0, at),
            placed,
            ...remaining.cards.slice(at),
          ],
        }
      : entry,
  );
}

/**
 * One card move, in exactly the terms `POST /cards/:id/move` accepts.
 *
 * A target column and an anchor — no index, no rank. Both the drag and the
 * keyboard produce this type and nothing else, so the request body is decided
 * by arithmetic over the board rather than by whichever gesture happened.
 */
export type CardMove = {
  cardId: string;
  columnId: string;
  /** The card this one goes after; null means first in the column. */
  afterCardId: string | null;
};

/** Where a card is, as a sentence's worth of facts. */
export type CardPosition = {
  columnName: string;
  /** 1-based, because it is read aloud rather than indexed with. */
  index: number;
  total: number;
};

/**
 * Locates a card for the benefit of a screen reader.
 *
 * A reorder that is only visible is invisible: nothing about a card moving up
 * one place fires an accessibility event, so the position has to be said. This
 * returns the facts; `board-view.tsx` turns them into the sentence, because
 * where the words live is a copy decision and where the card is is not.
 */
export function cardPosition(
  snapshot: BoardSnapshot,
  cardId: string,
): CardPosition | undefined {
  for (const entry of snapshot.columns) {
    const index = entry.cards.findIndex((card) => card.id === cardId);

    if (index !== -1) {
      return {
        columnName: entry.column.name,
        index: index + 1,
        total: entry.cards.length,
      };
    }
  }

  return undefined;
}

/**
 * The nearest anchor at or before `index` that the server would recognise.
 *
 * Cards the server has not acknowledged carry a `pending:` id, which is not a
 * uuid and would be answered with 400. They are also always at the bottom of
 * their column, because that is where `CreateCard` puts them — so walking back
 * to the last settled card is not a fudge, it is the true statement closest to
 * what the user pointed at: "after the last card that exists".
 *
 * Running off the front returns null, which is "first" and always valid.
 */
function anchorAt(cards: readonly Card[], index: number): string | null {
  for (let at = Math.min(index, cards.length - 1); at >= 0; at -= 1) {
    if (!isPendingId(cards[at].id)) {
      return cards[at].id;
    }
  }

  return null;
}

/** The column holding a card, with the card's index in it. */
function locate(
  snapshot: BoardSnapshot,
  cardId: string,
): { column: number; index: number } | undefined {
  for (const [column, entry] of snapshot.columns.entries()) {
    const index = entry.cards.findIndex((card) => card.id === cardId);

    if (index !== -1) {
      return { column, index };
    }
  }

  return undefined;
}

/** That column's cards with the moving one lifted out, as the anchor list. */
function without(cards: readonly Card[], cardId: string): Card[] {
  return cards.filter((card) => card.id !== cardId);
}

/**
 * Whether a move would leave the card exactly where it already is.
 *
 * Worth its own function because two of the three callers need it for different
 * reasons. A drag that ends where it started should not spend a request and a
 * whole-board re-read on a no-op; and `after_card_id` equal to the moving card's
 * own id is a **409** rather than a no-op, because `MoveCard`'s anchor CTE
 * excludes the moving row, so the anchor matches nothing. That one is not a
 * theoretical case — it is what "drop the card on itself" computes to.
 */
export function isRedundantMove(snapshot: BoardSnapshot, move: CardMove): boolean {
  if (move.afterCardId === move.cardId) {
    return true;
  }

  const at = locate(snapshot, move.cardId);

  if (at === undefined) {
    return true;
  }

  const entry = snapshot.columns[at.column];

  if (entry.column.id !== move.columnId) {
    return false;
  }

  return anchorAt(without(entry.cards, move.cardId), at.index - 1) === move.afterCardId;
}

/** Which way a card is being nudged by the keyboard. */
export type CardDirection = "up" | "down" | "left" | "right";

/**
 * The move one arrow key press makes, or undefined at the edges of the board.
 *
 * This is the whole keyboard reorder. Up and down walk the column; left and
 * right cross to the neighbouring one **keeping the card's row**, clamped to
 * however many cards that column has — which is what dropping a card sideways
 * looks like, and is predictable in a way "the nearest thing in that direction"
 * is not once columns scroll independently.
 *
 * Up is the case worth reading twice, and it is the same one
 * {@link moveAnchor} calls out for columns: to land *before* the card above,
 * the anchor is the card above **that**, two places back, and null when there
 * is none. An index would have been shorter to write and would have been a
 * claim about a list this client may already be wrong about.
 *
 * Every position in every column is reachable by repeating these, so the
 * keyboard reaches exactly the set of placements a drag does — and because each
 * press only moves the *proposal*, reaching a far corner still costs one
 * request, the same as one drag.
 */
export function cardNudge(
  snapshot: BoardSnapshot,
  cardId: string,
  direction: CardDirection,
): CardMove | undefined {
  const at = locate(snapshot, cardId);

  if (at === undefined) {
    return undefined;
  }

  const entry = snapshot.columns[at.column];

  if (direction === "up" || direction === "down") {
    const rest = without(entry.cards, cardId);
    const to = direction === "up" ? at.index - 1 : at.index + 1;

    if (to < 0 || to > rest.length) {
      return undefined;
    }

    return { cardId, columnId: entry.column.id, afterCardId: anchorAt(rest, to - 1) };
  }

  const to = direction === "left" ? at.column - 1 : at.column + 1;

  if (to < 0 || to >= snapshot.columns.length) {
    return undefined;
  }

  const into = snapshot.columns[to];

  return {
    cardId,
    columnId: into.column.id,
    // The same row, or the end of a shorter column.
    afterCardId: anchorAt(into.cards, Math.min(at.index, into.cards.length) - 1),
  };
}

/** What a drag is hovering: another card, or a column's empty space. */
export type DropOver = { card: string } | { column: string };

/**
 * The move a drop would make, given what the pointer is over.
 *
 * The gesture's whole contribution is `over` and `after` — which element, and
 * which side of it. Everything else is read off the board, which is why this is
 * a pure function and why the drag can be tested without a browser laying
 * anything out.
 *
 * **Within a column the anchor comes from the indices, not from `after`.** The
 * sortable library is already drawing the gap using `arrayMove` semantics —
 * dragging down lands you *after* the card you are over, dragging up lands you
 * *before* it — and committing anything else would put the card one place away
 * from the gap the user was looking at. Across columns there is no such gap to
 * match, because that preview is ours, so the pointer's own side of the card is
 * the better signal and `after` is used.
 *
 * Dropping on a column rather than a card is the empty space below the last
 * card, and appends. It is the only way to reach the bottom of a full column,
 * and the only way into an empty one.
 */
export function cardDropTarget(
  snapshot: BoardSnapshot,
  cardId: string,
  over: DropOver,
  after: boolean,
): CardMove | undefined {
  const at = locate(snapshot, cardId);

  if (at === undefined) {
    return undefined;
  }

  const from = snapshot.columns[at.column];

  if ("column" in over) {
    const into = snapshot.columns.find((entry) => entry.column.id === over.column);

    if (into === undefined) {
      return undefined;
    }

    const rest = without(into.cards, cardId);

    return {
      cardId,
      columnId: into.column.id,
      afterCardId: anchorAt(rest, rest.length - 1),
    };
  }

  if (over.card === cardId) {
    return undefined;
  }

  const into = snapshot.columns.find((entry) =>
    entry.cards.some((card) => card.id === over.card),
  );

  if (into === undefined) {
    return undefined;
  }

  const rest = without(into.cards, cardId);
  const anchor = rest.findIndex((card) => card.id === over.card);

  if (into.column.id === from.column.id) {
    const target = from.cards.findIndex((card) => card.id === over.card);
    const below = at.index < target;

    return {
      cardId,
      columnId: into.column.id,
      afterCardId: below ? anchorAt(rest, anchor) : anchorAt(rest, anchor - 1),
    };
  }

  return {
    cardId,
    columnId: into.column.id,
    afterCardId: after ? anchorAt(rest, anchor) : anchorAt(rest, anchor - 1),
  };
}

/** Which way a column is being nudged, for the keyboard-reachable reorder. */
export type MoveDirection = "left" | "right";

/**
 * The `after_column_id` that moves a column one place in `direction`.
 *
 * Returns `undefined` when the move is not available — the first column cannot
 * go left and the last cannot go right — so the caller disables the control
 * rather than sending a request the board already knows is meaningless.
 *
 * The left case is the one worth reading twice. To put a column *before* its
 * left-hand neighbour, the anchor is the column before *that* one, which is two
 * places back; and when there is no such column the anchor is null, meaning
 * "first". Expressing the move as an anchor rather than an index is what lets
 * the same argument survive somebody else reordering the board underneath it.
 */
export function moveAnchor(
  columns: readonly ColumnWithCards[],
  columnId: string,
  direction: MoveDirection,
): { afterColumnId: string | null } | undefined {
  const index = columns.findIndex((entry) => entry.column.id === columnId);

  if (index === -1) {
    return undefined;
  }

  if (direction === "left") {
    if (index === 0) {
      return undefined;
    }

    return { afterColumnId: index === 1 ? null : columns[index - 2].column.id };
  }

  if (index === columns.length - 1) {
    return undefined;
  }

  return { afterColumnId: columns[index + 1].column.id };
}
