"use client";

import Link from "next/link";
import { useCallback, useEffect, useOptimistic, useRef, useState } from "react";

import type { Card } from "@/lib/api/types";
import { type BoardChange, applyBoardChange, isPendingId } from "@/lib/board/mutations";
import type { BoardSnapshot, ColumnWithCards } from "@/lib/board/snapshot";
import { columnNameOf, findCard } from "@/lib/board/snapshot";
import { boardHref, cardHref } from "@/lib/workspace/routes";
import { FormMessage } from "@/components/workspace/fields";
import { CardComposer, ColumnActions, ColumnComposer } from "./board-controls";
import { CardDetail, CardNotOnBoard } from "./card-detail";
import styles from "./board.module.css";
import workspace from "@/components/workspace/workspace.module.css";

/**
 * The board: columns across, cards down, in the order the API returned them —
 * and, since #64, editable.
 *
 * # The Server/Client boundary, which #65 and #66 inherit
 *
 * This file is where the client begins, and `page.tsx` is still the only thing
 * that touches the API or the session. The page fetches four responses, checks
 * the two 404 cases, groups them with `lib/board/snapshot.ts`, and hands the
 * result down as plain serialisable props. Nothing below it fetches.
 *
 * #63 predicted this change and the prediction held: the props did not have to
 * move. `Card` and `Column` are strings all the way down, so a `"use client"`
 * at the top of this file was the whole of the boundary change.
 *
 * The three rules that came with it, still in force:
 *
 * 1. **`page.tsx` is not a Client Component.** The session and the token live
 *    on the server. A client page would refetch the board through `/api/proxy`
 *    on every mount and give up first paint for nothing.
 * 2. **No board component fetches.** Reads happen once, on the server, for the
 *    whole board. One request per column is the failure mode this design exists
 *    to avoid and it starts with one innocent `useEffect`.
 * 3. **Reorder by asking the server.** Moving a column posts
 *    `POST /columns/:id/move` with a neighbour's id and then re-reads. The
 *    splice in `lib/board/mutations.ts` is a *display* held for the duration of
 *    the request and then thrown away; it never becomes the source of truth,
 *    because ADR 0004 keeps the rank that defines the order off the wire and a
 *    client that invented one would be disagreeing with every other client.
 *
 * # One optimistic store, above everything that can be deleted
 *
 * `useOptimistic` lives here, and so does the failure message, because this
 * component is the only one guaranteed to outlive every edit. A control that
 * deletes a column is inside that column: it unmounts the instant the
 * optimistic delete applies, taking any state it held with it. So the controls
 * (`board-controls.tsx`, and the editor in `card-detail.tsx`) are handed
 * `applyChange` and `report` and own neither.
 *
 * That also puts the card detail panel inside this component rather than beside
 * it in `page.tsx`. It is the same store, so renaming a card in the panel moves
 * the tile behind it on the same frame — two stores would have shown the new
 * title in one place and the old one in the other until the refresh landed.
 *
 * # Editing controls mount when they are opened
 *
 * Every control here is a plain `<button>` until pressed. See the comment in
 * `board-controls.tsx` for why; the short version is that an 18rem column has
 * no room for six open forms, and that a board nobody is editing should mount
 * nothing that edits.
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
  /**
   * The `?card=` value, exactly as it arrived.
   *
   * Unresolved on purpose: an id naming nothing on this board is a state the
   * panel renders ("card not found"), so telling the two apart has to happen
   * where the board's own cards are known — which, once a card can be deleted
   * optimistically, is here rather than in the page.
   */
  selectedCardId: string | null;
}) {
  const [board, applyChange] = useOptimistic(snapshot, applyBoardChange);

  // Keyed so that the same message reported twice still moves focus: a second
  // failure that reads identically to the first is still news.
  const [failure, setFailure] = useState<{ message: string; key: number } | null>(null);
  const alertRef = useRef<HTMLDivElement | null>(null);

  const report = useCallback((message: string | null) => {
    setFailure((previous) =>
      message === null ? null : { message, key: (previous?.key ?? 0) + 1 },
    );
  }, []);

  useEffect(() => {
    if (failure !== null) {
      alertRef.current?.focus();
    }
  }, [failure]);

  const [addingColumn, setAddingColumn] = useState(false);

  const columns = board.columns.map((entry) => entry.column);
  const cards = [...board.columns.flatMap((entry) => entry.cards), ...board.unplaced];

  const openCard = selectedCardId === null ? null : findCard(cards, selectedCardId);
  const closeHref = boardHref(projectId, boardId);

  const wiring = { applyChange, report };

  return (
    <div
      className={
        selectedCardId === null
          ? styles.layout
          : `${styles.layout} ${styles.layoutWithDetail}`
      }
    >
      <div className={styles.boardArea}>
        {failure !== null && (
          <FormMessage messageRef={alertRef} title="That change was not saved">
            <p>{failure.message}</p>
          </FormMessage>
        )}

        {board.columns.length === 0 ? (
          <EmptyBoard
            adding={addingColumn}
            boardId={boardId}
            onClose={() => setAddingColumn(false)}
            onOpen={() => setAddingColumn(true)}
            wiring={wiring}
          />
        ) : (
          <ol className={styles.columns}>
            {board.columns.map((entry) => (
              <BoardColumn
                boardId={boardId}
                columns={board.columns}
                entry={entry}
                key={entry.column.id}
                projectId={projectId}
                selectedCardId={openCard?.id ?? null}
                wiring={wiring}
              />
            ))}

            <li className={styles.addColumn}>
              {addingColumn ? (
                <ColumnComposer
                  applyChange={applyChange}
                  boardId={boardId}
                  onClose={() => setAddingColumn(false)}
                  report={report}
                />
              ) : (
                // No `aria-expanded`: this button is *replaced* by the form
                // rather than disclosing a region it stays next to, and a
                // control that reports itself collapsed after being activated
                // describes something that is not on the page.
                <button
                  className={styles.addColumnButton}
                  onClick={() => setAddingColumn(true)}
                  type="button"
                >
                  + Add a column
                </button>
              )}
            </li>
          </ol>
        )}
      </div>

      {selectedCardId !== null && (
        <div className={styles.detailArea}>
          {openCard === null ? (
            <CardNotOnBoard closeHref={closeHref} />
          ) : (
            <CardDetail
              card={openCard}
              closeHref={closeHref}
              columnName={columnNameOf(columns, openCard)}
              editing={wiring}
              // Opening another card resets the panel rather than carrying a
              // half-finished edit across to it.
              key={openCard.id}
            />
          )}
        </div>
      )}
    </div>
  );
}

type Wiring = {
  applyChange: (change: BoardChange) => void;
  report: (message: string | null) => void;
};

/**
 * A board with no columns yet.
 *
 * Before #64 this said creating a column was not built and pointed at the API.
 * It is built, so the screen offers it: the explanation of what a column is for
 * is still the useful half, and the form is now the other half rather than an
 * apology.
 */
function EmptyBoard({
  boardId,
  adding,
  onOpen,
  onClose,
  wiring,
}: {
  boardId: string;
  adding: boolean;
  onOpen: () => void;
  onClose: () => void;
  wiring: Wiring;
}) {
  return (
    <div className={workspace.panel}>
      <h2 className={workspace.panelTitle}>This board has no columns yet</h2>

      <div className={workspace.panelBody}>
        <p>
          A board is columns you move cards between — <em>To do</em>, <em>Doing</em>,{" "}
          <em>Done</em> is the usual first three, but they are whatever the work
          actually looks like.
        </p>
      </div>

      {adding ? (
        <ColumnComposer
          applyChange={wiring.applyChange}
          boardId={boardId}
          onClose={onClose}
          report={wiring.report}
        />
      ) : (
        <button className={workspace.submit} onClick={onOpen} type="button">
          Add the first column
        </button>
      )}
    </div>
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
  columns,
  projectId,
  boardId,
  selectedCardId,
  wiring,
}: {
  entry: ColumnWithCards;
  columns: readonly ColumnWithCards[];
  projectId: string;
  boardId: string;
  selectedCardId: string | null;
  wiring: Wiring;
}) {
  const { column, cards } = entry;
  const headingId = `column-${column.id}`;

  const [editing, setEditing] = useState(false);
  const [adding, setAdding] = useState(false);

  // A column the server has not acknowledged has no id to address yet, so it is
  // shown without controls rather than with ones that would 404.
  const settled = !isPendingId(column.id);

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

      {settled && (
        <div className={styles.toolRow}>
          {/* The name is in the accessible label rather than in the text,
           * because six buttons all reading "Edit" are six identical entries in
           * a screen reader's element list. The visible word is a prefix of the
           * label, which is what WCAG's Label in Name asks for. */}
          <button
            aria-expanded={editing}
            aria-label={`${editing ? "Close" : "Edit"} ${column.name}`}
            className={styles.toolButton}
            onClick={() => setEditing((open) => !open)}
            type="button"
          >
            {editing ? "Close" : "Edit"}
          </button>
        </div>
      )}

      {settled && editing && (
        <ColumnActions
          applyChange={wiring.applyChange}
          columns={columns}
          entry={entry}
          onClose={() => setEditing(false)}
          report={wiring.report}
        />
      )}

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

      {settled &&
        (adding ? (
          <CardComposer
            applyChange={wiring.applyChange}
            boardId={boardId}
            columnId={column.id}
            columnName={column.name}
            onClose={() => setAdding(false)}
            report={wiring.report}
          />
        ) : (
          <button
            aria-label={`Add a card to ${column.name}`}
            className={styles.addCardButton}
            onClick={() => setAdding(true)}
            type="button"
          >
            <span aria-hidden="true">+ </span>Add a card
          </button>
        ))}
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
 *
 * A card the server has not acknowledged is **not** a link. Its id was invented
 * by this client, so `?card=<that id>` would open a panel for a card that does
 * not exist — and would keep working, confusingly, right up until the refresh
 * replaced the id with a real one.
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
  if (isPendingId(card.id)) {
    return (
      <span className={`${styles.card} ${styles.cardPending}`}>
        <span className={styles.cardTitle}>{card.title}</span>
        <span className={styles.cardPendingNote}>Adding…</span>
      </span>
    );
  }

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
