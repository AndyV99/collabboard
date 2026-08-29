/**
 * Turning somebody else's event into a change to the board on screen.
 *
 * This is the third of the three layers the board is drawn from, and the one
 * this issue adds:
 *
 * ```
 * snapshot (prop)        what the Server Component last read     the truth
 *   + live events        this file, applied in arrival order     seconds old
 *     + useOptimistic    this user's own unconfirmed edits       milliseconds old
 * ```
 *
 * Each layer is younger and less certain than the one below it, and each is
 * discarded when the layer below catches up. `board-view.tsx` composes them;
 * `use-board-live.ts` decides when the middle one is dropped.
 *
 * # Every function here is idempotent, and that is load-bearing
 *
 * The live layer is **replayed** over each new snapshot the server sends, not
 * applied once and forgotten. A replay is what stops a re-read that started
 * before an event arrived from silently undoing it: rather than reasoning about
 * whether a given read included a given write, the events are simply applied
 * again, and an event the read already reflects has to be a no-op.
 *
 * So {@link applyLiveEvent} is written so that applying it twice is the same as
 * applying it once:
 *
 * - **`*.created`** skips an entity already on the board. This is the only
 *   branch that needed changing from {@link applyBoardChange}'s behaviour —
 *   appending twice would draw the card twice.
 * - **`*.moved`** lifts the entity out and re-inserts it after a *named*
 *   neighbour, so the second application computes the same position as the
 *   first. This is ADR 0004's anchor paying off: an index would have been
 *   idempotent only by luck.
 * - **`*.updated`** assigns fields, and **`*.deleted`** filters. Both are
 *   naturally idempotent.
 *
 * The one thing replay cannot survive is an event that is *older* than the
 * snapshot it is replayed over and has since been contradicted by a change the
 * stream dropped. That is why the live layer is pruned rather than kept
 * forever, and `use-board-live.ts` states the rule that prunes it.
 */

import {
  type BoardChange,
  applyBoardChange,
} from "@/lib/board/mutations";
import type { BoardSnapshot } from "@/lib/board/snapshot";
import type { RealtimeEvent } from "./protocol";

/**
 * The board change an event implies, or null when it implies none.
 *
 * `board.updated` and `board.deleted` return null: neither changes the shape of
 * the columns-and-cards the live layer draws. They are handled by
 * `use-board-live.ts`, which re-reads for the first and stops for the second.
 */
export function changeFor(event: RealtimeEvent): BoardChange | null {
  switch (event.type) {
    case "card.created":
      return { kind: "card.created", columnId: event.card.columnId, card: event.card };

    case "card.updated":
      // Every field stated, none of them optional here. The payload is the
      // *whole* card — `patchCardHandler` publishes `newCardBody(card)` rather
      // than the fields that changed, precisely so a client can replace one
      // wholesale — so "leave it alone" is not a thing this event can mean, and
      // an unassignment arrives as the null it is.
      return {
        kind: "card.updated",
        cardId: event.card.id,
        title: event.card.title,
        description: event.card.description,
        assigneeId: event.card.assigneeId,
        dueAt: event.card.dueAt,
      };

    case "card.moved":
      return {
        kind: "card.moved",
        cardId: event.card.id,
        columnId: event.card.columnId,
        afterCardId: event.afterCardId,
      };

    case "card.deleted":
      return { kind: "card.deleted", cardId: event.cardId };

    case "column.created":
      return { kind: "column.created", column: event.column };

    case "column.updated":
      return { kind: "column.renamed", columnId: event.column.id, name: event.column.name };

    case "column.moved":
      return {
        kind: "column.moved",
        columnId: event.column.id,
        afterColumnId: event.afterColumnId,
      };

    case "column.deleted":
      return { kind: "column.deleted", columnId: event.columnId };

    case "board.updated":
    case "board.deleted":
      return null;
  }
}

/** Whether a card with this id is anywhere on the board, placed or not. */
function hasCard(snapshot: BoardSnapshot, cardId: string): boolean {
  return (
    snapshot.unplaced.some((card) => card.id === cardId) ||
    snapshot.columns.some((entry) => entry.cards.some((card) => card.id === cardId))
  );
}

/** Whether a column with this id is on the board. */
function hasColumn(snapshot: BoardSnapshot, columnId: string): boolean {
  return snapshot.columns.some((entry) => entry.column.id === columnId);
}

/**
 * Applies one event to the board, idempotently.
 *
 * Returns the snapshot unchanged when the event says nothing this layer can
 * draw — a `board.*` event, or a create for something already present.
 *
 * **A card whose column is not on this board is dropped rather than placed.**
 * `applyBoardChange`'s `card.created` looks for the named column and does
 * nothing when it is missing, which is right for an optimistic edit (the user
 * is looking at the column they clicked in) and would silently lose a card
 * here, where the column may simply not have been read yet. Losing it is still
 * the right answer — the re-read that follows every event brings both — but it
 * is worth knowing that this is where it happens.
 */
export function applyLiveEvent(
  snapshot: BoardSnapshot,
  event: RealtimeEvent,
): BoardSnapshot {
  if (event.type === "card.created" && hasCard(snapshot, event.card.id)) {
    return snapshot;
  }

  if (event.type === "column.created" && hasColumn(snapshot, event.column.id)) {
    return snapshot;
  }

  const change = changeFor(event);

  return change === null ? snapshot : applyBoardChange(snapshot, change);
}

/** Folds a whole live log over a snapshot, oldest event first. */
export function applyLiveLog(
  snapshot: BoardSnapshot,
  events: readonly RealtimeEvent[],
): BoardSnapshot {
  return events.reduce(applyLiveEvent, snapshot);
}
