import Link from "next/link";

import type { Card } from "@/lib/api/types";
import type { BoardSnapshot, ColumnWithCards } from "@/lib/board/snapshot";
import { cardHref } from "@/lib/workspace/routes";
import styles from "./board.module.css";

/**
 * The board: columns across, cards down, in the order the API returned them.
 *
 * # The Server/Client boundary, which #64–#66 inherit
 *
 * This component and everything under it is **pure and presentational**. It
 * takes a resolved {@link BoardSnapshot} and a couple of strings, performs no
 * fetch, holds no state, and has no `"use client"` — so today the whole board
 * is rendered on the server and the browser is sent no board code at all.
 *
 * That is deliberate rather than incidental, and the shape is the part that
 * matters more than the current directive:
 *
 * - **The page fetches, this renders.** `page.tsx` is the only thing that talks
 *   to the API. Adding a request in here would put a round trip inside a render
 *   and, worse, would be the first step towards one request per column.
 * - **The props are plain serialisable data.** `Card` and `Column` are strings
 *   all the way down (`lib/api/types.ts` keeps timestamps as strings for
 *   exactly this reason). So when #64 or #65 needs interaction, the change is
 *   `"use client"` at the top of *this* file — the props already cross an RSC
 *   boundary unchanged, and `page.tsx` does not move.
 * - **Order is never recomputed.** The snapshot arrives ordered and is rendered
 *   in `map` order. #65's drag-and-drop should keep that: reorder by asking the
 *   server (`POST /cards/:id/move` with an `after_card_id`) and re-reading, not
 *   by sorting an array here. See `lib/board/snapshot.ts` and ADR 0004.
 * - **Selection is a URL, not state.** The open card is a search parameter, so
 *   there is nothing to lift into a provider when this becomes a Client
 *   Component, and a reload still shows the same card.
 *
 * The one thing #64–#66 should *not* do is make `page.tsx` a Client Component to
 * get state into it. The session and the API token live on the server; a client
 * page would have to refetch the board through `/api/proxy` on every mount and
 * would give up first paint for nothing.
 *
 * # Read-only, and honestly so
 *
 * There is no add, edit, delete or drag here, and no disabled control standing
 * in for one. A greyed-out "New card" button is a promise the screen cannot
 * keep; the note under the board says plainly what is missing instead.
 */
export function BoardView({
  snapshot,
  projectId,
  boardId,
  selectedCardId,
}: {
  snapshot: BoardSnapshot;
  projectId: string;
  boardId: string;
  /** The card the URL has open, so its tile can be marked as current. */
  selectedCardId: string | null;
}) {
  return (
    <ol className={styles.columns}>
      {snapshot.columns.map((entry) => (
        <BoardColumn
          boardId={boardId}
          entry={entry}
          key={entry.column.id}
          projectId={projectId}
          selectedCardId={selectedCardId}
        />
      ))}
    </ol>
  );
}

/**
 * One column and its cards.
 *
 * The card list is an `<ol>`, not a `<ul>`: the order is the board's meaning,
 * not a rendering detail, and a screen reader announcing "list item 3 of 7" is
 * telling the user something true and useful about where the card sits.
 *
 * `tabIndex={0}` on that list is not decoration either — it is what makes a
 * column with more cards than fit scrollable without a mouse. The list also
 * carries its own accessible name, so landing in it announces which column it
 * belongs to rather than "list".
 */
function BoardColumn({
  entry,
  projectId,
  boardId,
  selectedCardId,
}: {
  entry: ColumnWithCards;
  projectId: string;
  boardId: string;
  selectedCardId: string | null;
}) {
  const { column, cards } = entry;
  const headingId = `column-${column.id}`;

  return (
    <li aria-labelledby={headingId} className={styles.column}>
      <div className={styles.columnHeader}>
        <h3 className={styles.columnName} id={headingId}>
          {column.name}
        </h3>

        <span className={styles.columnCount}>
          {cards.length} {cards.length === 1 ? "card" : "cards"}
        </span>
      </div>

      {cards.length === 0 ? (
        <p className={styles.columnEmpty}>No cards in this column.</p>
      ) : (
        <ol aria-labelledby={headingId} className={styles.stack} tabIndex={0}>
          {cards.map((card) => (
            <li key={card.id}>
              <BoardCard
                boardId={boardId}
                card={card}
                projectId={projectId}
                selected={card.id === selectedCardId}
              />
            </li>
          ))}
        </ol>
      )}
    </li>
  );
}

/**
 * One card, as a link that opens it.
 *
 * A link rather than a button because opening a card *is* a navigation here —
 * it changes the URL and the URL is the state. That also means it works before
 * hydration, opens in a new tab on a modifier-click, and can be copied.
 *
 * `aria-current="true"` on the open card, so the selection is announced rather
 * than only outlined.
 */
function BoardCard({
  card,
  projectId,
  boardId,
  selected,
}: {
  card: Card;
  projectId: string;
  boardId: string;
  selected: boolean;
}) {
  return (
    <Link
      aria-current={selected ? "true" : undefined}
      className={selected ? `${styles.card} ${styles.cardSelected}` : styles.card}
      href={cardHref(projectId, boardId, card.id)}
    >
      <span className={styles.cardTitle}>{card.title}</span>

      {card.description !== "" && (
        <span className={styles.cardBody}>{card.description}</span>
      )}
    </Link>
  );
}
