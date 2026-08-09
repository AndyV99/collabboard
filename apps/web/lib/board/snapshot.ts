/**
 * Turning three API responses into one board, without inventing an order.
 *
 * `GET /boards/:id/columns` and `GET /boards/:id/cards` each answer with a flat
 * list. The board is the two of them crossed: every card sits under the column
 * whose id it names, and every column keeps the cards it was given in the order
 * it was given them. That is the whole job of this file, and the interesting
 * part is what it deliberately does *not* do.
 *
 * # There is no sort in here, and there must never be one
 *
 * ADR 0004 decided that a card's rank is a server-allocated `numeric` that
 * appears in no response and is accepted by no endpoint. `apps/api` orders
 * `ListCardsByBoard` with `ORDER BY column_id, position, id` and
 * `ListColumnsByBoard` with `ORDER BY position, id`, so **the sequence of the
 * array is the answer**. There is nothing else in the payload to sort by:
 * `created_at` is the order cards were *made*, which stops matching the board
 * the first time anyone drags anything, and `title` is not an order at all.
 *
 * So {@link groupCardsIntoColumns} is a stable single pass that preserves input
 * order and computes nothing. If a future change here reaches for
 * `.sort(...)`, the fix is upstream — either the API's `ORDER BY` is wrong or
 * the request was not the one the screen needed.
 *
 * This matters past correctness. #65 adds drag-and-drop against an
 * anchor-based API (`after_card_id`, with null meaning first), and an anchor is
 * only meaningful against the list the server actually has. A client carrying
 * its own ordering model would be computing anchors from a list nobody else
 * agrees with.
 *
 * # Why the cross is done here and not in a component
 *
 * It is pure, it takes plain data, and it returns plain data — so it is a unit
 * test rather than a rendering that has to be provoked, and it can be called
 * from a Server Component today and from a Client Component in #64–#66 without
 * moving. The board's shape is decided in one function that neither renders nor
 * fetches.
 */

import type { Card, Column } from "@/lib/api/types";

/** One column and the cards under it, both in the API's order. */
export type ColumnWithCards = {
  column: Column;
  /** This column's cards, in the sequence `GET /boards/:id/cards` returned. */
  cards: Card[];
};

/** A board as the screen renders it. */
export type BoardSnapshot = {
  /** Every column, in the sequence `GET /boards/:id/columns` returned. */
  columns: ColumnWithCards[];

  /**
   * Cards naming a column that was not in the columns response.
   *
   * Not a theoretical case, and not an error. The two lists are two requests,
   * and nothing makes them one transaction: a column created between them is
   * absent from the first response while its cards are present in the second.
   * The realtime layer (#9) will close most of that window and cannot close all
   * of it.
   *
   * These are surfaced rather than dropped. Silently discarding a card the
   * server sent would make the board quietly wrong — the one failure a Kanban
   * tool cannot afford — and dropping it in some arbitrary column would be
   * worse, because it would look right.
   */
  unplaced: Card[];
};

/**
 * Crosses the columns list with the cards list.
 *
 * One pass over the cards to bucket them, one pass over the columns to emit
 * them in the order they arrived — O(columns + cards), no comparison of any
 * card against any other. Cards keep their relative order within a bucket
 * because they are appended in the order they are read.
 *
 * `cards` is read as the whole board's cards. Calling this with one column's
 * cards and every column would put every other column's cards in `unplaced`,
 * which is correct but is not the call anybody wants: the board view issues one
 * `GET /boards/:id/cards`, never one request per column.
 */
export function groupCardsIntoColumns(
  columns: readonly Column[],
  cards: readonly Card[],
): BoardSnapshot {
  const buckets = new Map<string, Card[]>();

  for (const column of columns) {
    buckets.set(column.id, []);
  }

  const unplaced: Card[] = [];

  for (const card of cards) {
    const bucket = buckets.get(card.columnId);

    if (bucket === undefined) {
      unplaced.push(card);
      continue;
    }

    bucket.push(card);
  }

  return {
    columns: columns.map((column) => ({
      column,
      // Present for every column, because the loop above seeded one per column.
      // The fallback is for the type checker, not for a case that can happen.
      cards: buckets.get(column.id) ?? [],
    })),
    unplaced,
  };
}

/** How many cards are on the board, unplaced ones included. */
export function countCards(snapshot: BoardSnapshot): number {
  return (
    snapshot.unplaced.length +
    snapshot.columns.reduce((total, entry) => total + entry.cards.length, 0)
  );
}

/**
 * Finds one card in a list already in memory.
 *
 * The card detail view is a lookup, not a request. `GET /boards/:id/cards`
 * returned every field `GET /cards/:id` would have — they render the same
 * `cardBody` — so fetching the selected card again would be a round trip for
 * bytes the page is already holding, and it would put the detail panel one
 * network failure away from a board that loaded fine.
 *
 * Returns null for an id that is not on this board, which is what a stale or
 * hand-edited `?card=` produces. The screen says so rather than guessing.
 */
export function findCard(cards: readonly Card[], cardId: string): Card | null {
  return cards.find((card) => card.id === cardId) ?? null;
}

/** The name of the column a card sits in, or null when it is not shown. */
export function columnNameOf(columns: readonly Column[], card: Card): string | null {
  return columns.find((column) => column.id === card.columnId)?.name ?? null;
}
