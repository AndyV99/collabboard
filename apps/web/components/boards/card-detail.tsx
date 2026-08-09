import Link from "next/link";

import type { Card } from "@/lib/api/types";
import { formatDateTime } from "@/lib/workspace/format";
import styles from "./board.module.css";

/**
 * One card, in full.
 *
 * # Every field the API exposes, and nothing it does not
 *
 * `cardBody` in `apps/api/internal/api/cards.go` is `id`, `board_id`,
 * `column_id`, `title`, `description`, `created_at`, `updated_at`. All seven
 * are accounted for here, three of them by being somewhere better than a row in
 * a table:
 *
 * - **`title`** is the heading, **`description`** the body, **`created_at`**
 *   and **`updated_at`** the two facts underneath.
 * - **`column_id`** is shown as the column's *name*, resolved from the columns
 *   the page already has. An opaque uuid tells a user nothing; the column is
 *   the one piece of a card's state that is not visible from the card itself.
 * - **`board_id`** is the board you are looking at. Rendering it would be
 *   restating the page you are on.
 * - **`id`** is in the address bar, because the open card is a search
 *   parameter. Copying the URL is how you send someone this card.
 *
 * **There is no position, rank or index**, because there is no such field in
 * any response and there is not meant to be — ADR 0004 keeps the rank off the
 * wire so no client can come to depend on a number that renumbering changes.
 * "Third in Doing" is a fact about the column, and the column is what shows it.
 *
 * # Not a dialog
 *
 * This is a panel beside the board, not a modal. A modal would need a focus
 * trap, an escape handler and an inert background — three pieces of client-side
 * behaviour for a read-only view of seven fields — and it would hide the board
 * you opened the card to see. When #64 makes this editable it becomes a form in
 * the same place, which is a smaller change than unpicking a dialog.
 *
 * The `id="card"` is what the board's card links point their fragment at, so
 * that on a narrow screen — where this panel sits above the board rather than
 * beside it — following a card brings the panel into view with no JavaScript.
 */
export function CardDetail({
  card,
  columnName,
  closeHref,
}: {
  card: Card;
  /** The name of the column the card is in, or null when it is not shown. */
  columnName: string | null;
  /** The board without this card open — where the close link goes. */
  closeHref: string;
}) {
  const created = formatDateTime(card.createdAt);
  const updated = formatDateTime(card.updatedAt);

  return (
    <article aria-labelledby="card-title" className={styles.detail} id="card">
      <div className={styles.detailHeader}>
        <h2 className={styles.detailTitle} id="card-title">
          {card.title}
        </h2>

        <Link className={styles.detailClose} href={closeHref}>
          Close
        </Link>
      </div>

      {card.description === "" ? (
        <p className={styles.detailEmpty}>This card has no description.</p>
      ) : (
        <p className={styles.detailDescription}>{card.description}</p>
      )}

      <dl className={styles.detailFacts}>
        <dt className={styles.detailTerm}>Column</dt>
        <dd className={styles.detailValue}>
          {/* Null means the card named a column the columns response did not
           * contain — the two-requests-are-not-one-snapshot case. Saying so is
           * better than printing a uuid or leaving the row blank. */}
          {columnName ?? "Not in a column shown on this board"}
        </dd>

        {created !== null && (
          <>
            <dt className={styles.detailTerm}>Created</dt>
            <dd className={styles.detailValue}>{created}</dd>
          </>
        )}

        {updated !== null && (
          <>
            <dt className={styles.detailTerm}>Updated</dt>
            <dd className={styles.detailValue}>{updated}</dd>
          </>
        )}
      </dl>
    </article>
  );
}

/**
 * What `?card=` shows when it names nothing on this board.
 *
 * Reachable three ways, none of them exotic: a link followed after the card was
 * deleted, a link to a card on a *different* board, and a hand-edited URL. All
 * three are the same answer, and — as with a 404 from the API — the screen does
 * not try to say which, because a message that distinguished "deleted" from
 * "belongs to another workspace" would be answering a question about another
 * tenant's data.
 *
 * No request is made to find out, either. The board's cards are already in
 * memory; a card that is not among them is not on this board, and asking
 * `GET /cards/:id` would only turn a wrong link into a second round trip.
 */
export function CardNotOnBoard({ closeHref }: { closeHref: string }) {
  return (
    <article className={styles.detail} id="card">
      <div className={styles.detailHeader}>
        <h2 className={styles.detailTitle}>Card not found</h2>

        <Link className={styles.detailClose} href={closeHref}>
          Close
        </Link>
      </div>

      <p className={styles.detailEmpty}>
        The link you followed names a card that is not on this board. It may have been
        deleted, or it may belong to a board you cannot see. The board itself loaded
        fine — it is behind this panel.
      </p>
    </article>
  );
}
