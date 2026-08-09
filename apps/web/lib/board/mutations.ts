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
