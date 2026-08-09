"use client";

import { useCallback, useEffect, useId, useOptimistic, useRef, useState } from "react";

import {
  type BoardChange,
  type CardDirection,
  type CardMove,
  applyBoardChange,
  cardDropTarget,
  cardNudge,
  cardPosition,
  isPendingId,
  isRedundantMove,
} from "@/lib/board/mutations";
import type { BoardSnapshot, ColumnWithCards } from "@/lib/board/snapshot";
import { columnNameOf, findCard } from "@/lib/board/snapshot";
import { boardHref } from "@/lib/workspace/routes";
import { FormMessage } from "@/components/workspace/fields";
import { CardComposer, ColumnActions, ColumnComposer } from "./board-controls";
import { CardDetail, CardNotOnBoard } from "./card-detail";
import { CardDragArea, CardDropZone, DraggableCard, PendingCard } from "./card-drag";
import type { DragReport } from "./card-drag";
import { useCardMoves } from "./card-moves";
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
 *
 * #65 is the exception, and it ends a property #64 had: a board nobody is
 * editing used to mount nothing router-bound, so `__tests__/board-view.test.tsx`
 * could render it with no app-router context at all. A card is draggable
 * without anyone opening anything, so the drag context and the mutation runner
 * behind it are mounted for every board. That test now mocks
 * `next/navigation` — which is the honest consequence of the feature, not a
 * test being bent to fit.
 *
 * # One proposed move, two ways to make it
 *
 * A drag and a keyboard lift are the same interaction: pick a card up, choose
 * somewhere, commit or give up. So they share one piece of state — `proposal`,
 * a {@link CardMove} — and the board is drawn by running it through
 * `applyBoardChange`, the same pure reducer the optimistic store uses. The two
 * input methods differ only in what sets it: a pointer over a card, or an arrow
 * key.
 *
 * That is why moving a card five places with the keyboard costs one request,
 * exactly like one drag. Each arrow key moves the *proposal*, which is local and
 * free; only the drop sends anything. A control that posted per keypress would
 * be five moves, five whole-board re-reads, and five chances for someone else's
 * edit to land in the middle of one gesture.
 *
 * `proposal` is plain `useState` and not part of the optimistic store on
 * purpose. It is a question, not an edit — nothing has been sent, and there is
 * nothing for the server to confirm or refuse until the card is dropped.
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

  const sendMove = useCardMoves(applyChange, report);

  /** Where a card would go if it were dropped now. Nothing has been sent. */
  const [proposal, setProposal] = useState<CardMove | null>(null);
  /** The card held by the keyboard, if any. Null during a pointer drag. */
  const [lifted, setLifted] = useState<string | null>(null);
  /** The card under the pointer, for the floating copy that follows it. */
  const [dragging, setDragging] = useState<string | null>(null);
  const [announcement, setAnnouncement] = useState("");

  const instructionsId = `${useId()}-move-help`;

  // The board as the user is looking at it: the server's, plus every optimistic
  // edit React is holding, plus the move being proposed. Only the last of those
  // is undecided, and it is undone by setting it back to null.
  const shown = proposal === null ? board : applyBoardChange(board, { kind: "card.moved", ...proposal });

  const columns = shown.columns.map((entry) => entry.column);
  const cards = [...shown.columns.flatMap((entry) => entry.cards), ...shown.unplaced];

  const openCard = selectedCardId === null ? null : findCard(cards, selectedCardId);
  const closeHref = boardHref(projectId, boardId);

  const wiring = { applyChange, report };

  const titleOf = (cardId: string): string =>
    cards.find((card) => card.id === cardId)?.title ?? "The card";

  /**
   * Sends a move, unless it would change nothing.
   *
   * `board` rather than `shown` is what "nothing" is measured against: `shown`
   * already has the proposal applied, so every move looks redundant against it.
   * The comparison that matters is against the board the server last confirmed
   * plus whatever is already in flight, which is exactly the optimistic value.
   */
  function commit(move: CardMove): void {
    if (isRedundantMove(board, move)) {
      say(`${titleOf(move.cardId)} was not moved. ${whereIs(shown, move.cardId)}`);

      return;
    }

    sendMove(move);
    say(
      `${titleOf(move.cardId)} moved. ${whereIs(
        applyBoardChange(board, { kind: "card.moved", ...move }),
        move.cardId,
      )}`,
    );
  }

  function say(message: string): void {
    // A live region whose text has not changed is not re-announced, and two
    // arrow presses that both hit the top of a column are two separate answers
    // to two separate questions. A zero-width space alternates on and off so
    // consecutive identical sentences are two different strings, and reads as
    // nothing.
    setAnnouncement((previous) => (previous === message ? `${message}​` : message));
  }

  function handleDragStart(cardId: string): void {
    setDragging(cardId);
    setLifted(null);
    setProposal(null);
    say(`${titleOf(cardId)} picked up. ${whereIs(shown, cardId)}`);
  }

  /**
   * Relocates the card while it is being dragged, but only across columns.
   *
   * Within a column the sortable strategy is already opening a gap where the
   * card would land, so re-ordering the list underneath it too would move the
   * card twice for one gesture. Crossing into another column has no such
   * preview — the card would otherwise hover over a list that has not made room
   * for it — so that one is drawn here.
   */
  // `drag`, not `report`: `report` is this component's failure channel, and
  // shadowing it here would hide it from the two functions likeliest to need it.
  function handleDragOver(drag: DragReport): void {
    const target = cardDropTarget(shown, drag.cardId, drag.over, drag.past);

    if (target === undefined || target.columnId === columnIdOf(shown, drag.cardId)) {
      return;
    }

    setProposal(target);
  }

  function handleDrop(drag: DragReport | null): void {
    const heldCard = dragging;
    const heldMove = proposal;

    setDragging(null);
    setProposal(null);

    if (drag === null) {
      // Let go outside every column. The card goes back — and says so, for
      // anyone who cannot watch it go back.
      if (heldCard !== null) {
        say(`${titleOf(heldCard)} was not moved. ${whereIs(board, heldCard)}`);
      }

      return;
    }

    // Falling back to the proposal is not belt-and-braces, it is the common
    // case for a drag that crossed columns — and getting it wrong is what made
    // every cross-column drop silently do nothing the first time this ran
    // against a real board. The preview has already drawn the card where it
    // would land, so by the time the button comes up the pointer is over *the
    // dragged card itself*; `cardDropTarget` rightly has no answer for that,
    // because `after_card_id` equal to the moving card's own id is a 409. The
    // proposal is what the user is looking at, and it is what they meant.
    const target =
      cardDropTarget(shown, drag.cardId, drag.over, drag.past) ?? heldMove;

    if (target === null) {
      // Picked up and put back down inside its own column, which is the drag
      // that changes nothing. Announced for the same reason the keyboard's
      // version of it is: silence is indistinguishable from a broken control.
      if (heldCard !== null) {
        say(`${titleOf(heldCard)} was not moved. ${whereIs(board, heldCard)}`);
      }

      return;
    }

    commit(target);
  }

  function handleDragCancel(): void {
    const held = dragging;

    setDragging(null);
    setProposal(null);

    if (held !== null) {
      say(`Move cancelled. ${titleOf(held)} is back in ${whereIs(board, held)}`);
    }
  }

  function handleLift(cardId: string): void {
    setLifted(cardId);
    setProposal(null);
    say(
      `${titleOf(cardId)} lifted. ${whereIs(shown, cardId)} Use the arrow keys to move it, ` +
        "Enter to drop it, Escape to cancel.",
    );
  }

  function handleNudge(cardId: string, direction: CardDirection): void {
    const target = cardNudge(shown, cardId, direction);

    if (target === undefined) {
      say(`${titleOf(cardId)} cannot move ${direction} from here. ${whereIs(shown, cardId)}`);

      return;
    }

    setProposal(target);
    say(
      whereIs(applyBoardChange(shown, { kind: "card.moved", ...target }), cardId),
    );
  }

  function handleKeyboardDrop(): void {
    const held = proposal;
    const card = lifted;

    setLifted(null);
    setProposal(null);

    if (held === null) {
      // Lifted and dropped without moving. Saying so matters more here than
      // anywhere else on this screen: for someone who cannot see the card, an
      // unremarked drop is indistinguishable from a key that did nothing, and
      // the next arrow press would be aimed at a card they no longer hold.
      if (card !== null) {
        say(`${titleOf(card)} was not moved. ${whereIs(shown, card)}`);
      }

      return;
    }

    commit(held);
  }

  function handleKeyboardCancel(): void {
    const held = lifted;

    setLifted(null);
    setProposal(null);

    if (held !== null) {
      say(`Move cancelled. ${titleOf(held)} is back in ${whereIs(board, held)}`);
    }
  }

  const moving = {
    instructionsId,
    lifted,
    onCancel: handleKeyboardCancel,
    onDrop: handleKeyboardDrop,
    onLift: handleLift,
    onNudge: handleNudge,
  };

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

        {/*
         * One live region for both input methods. `assertive`, because a
         * position that arrives after the next keypress is describing a board
         * that has already moved on; and `atomic`, so the whole sentence is
         * read rather than the words that happen to have changed.
         */}
        {/*
         * Named, because it is not the only one on the page: `DndContext`
         * renders a live region of its own that this board keeps silent (see
         * `card-drag.tsx`). A name is what distinguishes the region that speaks
         * from the one that does not, for a test and for anyone browsing
         * regions with a screen reader.
         */}
        <div
          aria-atomic="true"
          aria-label="Card moves"
          aria-live="assertive"
          className={styles.visuallyHidden}
          role="status"
        >
          {announcement}
        </div>

        <p className={styles.visuallyHidden} id={instructionsId}>
          Press Enter or Space to lift this card. Use the arrow keys to move it
          up and down its column or into the column beside it, then press Enter
          to drop it or Escape to leave it where it was.
        </p>

        {shown.columns.length === 0 ? (
          <EmptyBoard
            adding={addingColumn}
            boardId={boardId}
            onClose={() => setAddingColumn(false)}
            onOpen={() => setAddingColumn(true)}
            wiring={wiring}
          />
        ) : (
          <CardDragArea
            onCancel={handleDragCancel}
            onDrop={handleDrop}
            onOver={handleDragOver}
            onStart={handleDragStart}
            overlay={dragging === null ? null : (findCard(cards, dragging) ?? null)}
          >
            <ol className={styles.columns}>
              {shown.columns.map((entry) => (
                <BoardColumn
                  boardId={boardId}
                  columns={shown.columns}
                  entry={entry}
                  key={entry.column.id}
                  moving={moving}
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
          </CardDragArea>
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

/** The keyboard move, wired down to each card's grip. */
type Moving = {
  instructionsId: string;
  /** The card currently held by the keyboard, if it is this one. */
  lifted: string | null;
  onLift: (cardId: string) => void;
  onNudge: (cardId: string, direction: CardDirection) => void;
  onDrop: () => void;
  onCancel: () => void;
};

/**
 * Where a card is, as the end of a spoken sentence: "Doing, 2 of 3."
 *
 * "2 of 3" rather than "second" because a position is only meaningful next to
 * the length — "second" tells you nothing about whether the card is near the
 * bottom, which is the thing someone moving it wants to know.
 */
function whereIs(snapshot: BoardSnapshot, cardId: string): string {
  const at = cardPosition(snapshot, cardId);

  return at === undefined ? "" : `${at.columnName}, ${at.index} of ${at.total}.`;
}

/** Which column a card is currently drawn in, if any. */
function columnIdOf(snapshot: BoardSnapshot, cardId: string): string | undefined {
  return snapshot.columns.find((entry) =>
    entry.cards.some((card) => card.id === cardId),
  )?.column.id;
}

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
  moving,
}: {
  entry: ColumnWithCards;
  columns: readonly ColumnWithCards[];
  projectId: string;
  boardId: string;
  selectedCardId: string | null;
  wiring: Wiring;
  moving: Moving;
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

      <CardDropZone cards={cards} columnId={column.id} labelledBy={headingId}>
        {cards.map((card) =>
          isPendingId(card.id) ? (
            <PendingCard card={card} key={card.id} />
          ) : (
            <DraggableCard
              boardId={boardId}
              card={card}
              instructionsId={moving.instructionsId}
              key={card.id}
              lifted={moving.lifted === card.id}
              onCancel={moving.onCancel}
              onDrop={moving.onDrop}
              onLift={() => moving.onLift(card.id)}
              onNudge={(direction) => moving.onNudge(card.id, direction)}
              projectId={projectId}
              selected={card.id === selectedCardId}
            />
          ),
        )}
      </CardDropZone>

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
