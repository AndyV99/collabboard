"use client";

import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { createCard, createColumn, deleteColumn, moveColumn, updateColumn } from "@/lib/api/endpoints";
import type { Card, Column } from "@/lib/api/types";
import type { ColumnWithCards } from "@/lib/board/snapshot";
import { type BoardChange, type MoveDirection, moveAnchor, pendingId } from "@/lib/board/mutations";
import {
  MAX_NAME_CODE_POINTS,
  maxLengthFor,
  validateCardTitle,
  validateName,
} from "@/lib/workspace/rules";
import { TextField } from "@/components/workspace/fields";
import { useBoardMutation } from "./board-mutation";
import styles from "./board.module.css";
import workspace from "@/components/workspace/workspace.module.css";

/**
 * The board's editing controls.
 *
 * # Why every one of these mounts on demand
 *
 * None of these components is rendered until the user opens it. `BoardView`
 * renders a plain `<button>` and swaps in the form when it is pressed, rather
 * than rendering a form per column and hiding it.
 *
 * Three reasons, in descending order of how much they matter:
 *
 * 1. **A column is 18rem wide.** Six always-open forms is not a board, it is a
 *    settings page. Progressive disclosure is what every Kanban tool converged
 *    on for the same reason, and `components/projects/archive-project.tsx`
 *    already establishes the pattern in this app.
 * 2. **Nothing router-bound loads until it is needed.** These are the only
 *    components on the board that call `useRouter`, so a board nobody edits
 *    mounts none of them.
 * 3. It is why #63's `__tests__/board-view.test.tsx` still passes untouched: it
 *    renders the board without an app-router context, and a control that only
 *    mounts on a click never asks for one.
 *
 * # They write to a store they do not own
 *
 * Each takes `applyChange` (the board's `useOptimistic` setter) and `report`
 * (its failure setter). Both belong to `BoardView` because a destructive edit
 * unmounts the control that made it — deleting a column takes its own delete
 * button off the screen — so neither the optimistic value nor the error message
 * can live in the thing being deleted.
 */

type Wiring = {
  /** The board's optimistic reducer setter. */
  applyChange: (change: BoardChange) => void;
  /** The board's failure reporter. */
  report: (message: string | null) => void;
  /** Closes this control. */
  onClose: () => void;
};

/** `new Date().toISOString()`, as the API would render it. */
function now(): string {
  return new Date().toISOString();
}

/**
 * Adds a card to the bottom of one column.
 *
 * Title only. A card is *written* in the detail panel, where there is room for
 * a description and the 10,000 characters the API allows; the composer is for
 * getting a thought onto the board before it evaporates, which is what a board
 * is for. `POST /columns/:id/cards` takes an optional description and this
 * simply does not send one, so the card arrives with `""` — the same value the
 * API would have stored for a blank field.
 *
 * It stays open after a successful add, with the field cleared and focused, so
 * three cards is three sentences and not three trips through a disclosure.
 */
export function CardComposer({
  columnId,
  boardId,
  columnName,
  applyChange,
  report,
  onClose,
}: Wiring & { columnId: string; boardId: string; columnName: string }) {
  const titleId = `${useId()}-title`;
  const [title, setTitle] = useState("");
  const [error, setError] = useState<string | undefined>(undefined);
  const inputRef = useRef<HTMLInputElement | null>(null);

  const { run, pending } = useBoardMutation(applyChange, report);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending) {
      return;
    }

    const problem = validateCardTitle(title);

    setError(problem);

    if (problem !== undefined) {
      return;
    }

    const typed = title.trim();

    // Cleared before the transition starts, so the field is empty on the same
    // frame the card appears. A `useState` setter called inside the transition
    // would be deferred to the end of it, which on a slow connection is a
    // second of typing into a box that still holds the last card's title.
    setTitle("");

    const card: Card = {
      id: pendingId(),
      boardId,
      columnId,
      title: typed,
      description: "",
      createdAt: now(),
      updatedAt: now(),
    };

    run({
      change: { kind: "card.created", columnId, card },
      endpoint: createCard(columnId, { title: typed }),
      subject: "add the card",
      onSuccess: () => inputRef.current?.focus(),
      // The text goes back in the box. Losing what someone typed because the
      // network did not answer is the one failure a form must not have.
      onFailure: () => setTitle(typed),
    });
  }

  return (
    <form className={styles.composer} noValidate onSubmit={handleSubmit}>
      <TextField
        disabled={pending}
        error={error}
        id={titleId}
        inputRef={inputRef}
        label={`New card in ${columnName}`}
        maxLength={maxLengthFor(MAX_NAME_CODE_POINTS)}
        onChange={setTitle}
        value={title}
      />

      <div className={styles.composerActions}>
        <button className={workspace.submit} disabled={pending} type="submit">
          {pending ? "Adding…" : "Add card"}
        </button>

        <button
          className={workspace.secondary}
          onClick={onClose}
          type="button"
        >
          Done
        </button>
      </div>
    </form>
  );
}

/** Adds a column to the right-hand end of the board. */
export function ColumnComposer({
  boardId,
  applyChange,
  report,
  onClose,
}: Wiring & { boardId: string }) {
  const nameId = `${useId()}-name`;
  const [name, setName] = useState("");
  const [error, setError] = useState<string | undefined>(undefined);
  const inputRef = useRef<HTMLInputElement | null>(null);

  const { run, pending } = useBoardMutation(applyChange, report);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending) {
      return;
    }

    const problem = validateName(name, "Column");

    setError(problem);

    if (problem !== undefined) {
      return;
    }

    const typed = name.trim();

    setName("");

    const column: Column = {
      id: pendingId(),
      boardId,
      name: typed,
      createdAt: now(),
      updatedAt: now(),
    };

    run({
      change: { kind: "column.created", column },
      endpoint: createColumn(boardId, { name: typed }),
      subject: "add the column",
      onSuccess: () => inputRef.current?.focus(),
      onFailure: () => setName(typed),
    });
  }

  return (
    <form className={styles.composer} noValidate onSubmit={handleSubmit}>
      <TextField
        disabled={pending}
        error={error}
        hint="To do, Doing, Done — or whatever the work actually looks like."
        id={nameId}
        inputRef={inputRef}
        label="New column name"
        maxLength={maxLengthFor(MAX_NAME_CODE_POINTS)}
        onChange={setName}
        value={name}
      />

      <div className={styles.composerActions}>
        <button className={workspace.submit} disabled={pending} type="submit">
          {pending ? "Adding…" : "Add column"}
        </button>

        <button className={workspace.secondary} onClick={onClose} type="button">
          Done
        </button>
      </div>
    </form>
  );
}

/**
 * Rename, reorder and delete, for one column.
 *
 * # Reordering asks the server where the column goes
 *
 * The two buttons send `POST /columns/:id/move` with an `after_column_id` and
 * then re-read the board. They do not sort, and they could not: ADR 0004 keeps
 * the rank that defines the order off the wire entirely, so the client has
 * nothing to order by and is not meant to. `moveAnchor` turns "one to the left"
 * into the neighbour id the API actually accepts, and returns undefined at the
 * ends of the board so the button is disabled rather than sending a request
 * whose answer is already known.
 *
 * Buttons rather than dragging because dragging is #65 and because a board that
 * can only be reordered by pointer cannot be reordered by everyone. These stay
 * afterwards as the keyboard path.
 */
export function ColumnActions({
  entry,
  columns,
  applyChange,
  report,
  onClose,
}: Wiring & { entry: ColumnWithCards; columns: readonly ColumnWithCards[] }) {
  const nameId = `${useId()}-name`;
  const { column, cards } = entry;

  const [name, setName] = useState(column.name);
  const [error, setError] = useState<string | undefined>(undefined);
  const [confirming, setConfirming] = useState(false);

  const { run, pending } = useBoardMutation(applyChange, report);
  const confirmRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    if (confirming) {
      confirmRef.current?.focus();
    }
  }, [confirming]);

  function handleRename(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending) {
      return;
    }

    const problem = validateName(name, "Column");

    setError(problem);

    if (problem !== undefined) {
      return;
    }

    const typed = name.trim();

    // `PATCH /columns/:id` refuses a body that changes nothing, so an untouched
    // form would collect a 400 for doing nothing. Closing is the honest answer:
    // the column already has that name.
    if (typed === column.name) {
      onClose();

      return;
    }

    run({
      change: { kind: "column.renamed", columnId: column.id, name: typed },
      endpoint: updateColumn(column.id, { name: typed }),
      subject: "rename this column",
      onSuccess: onClose,
    });
  }

  function handleMove(direction: MoveDirection) {
    const anchor = moveAnchor(columns, column.id, direction);

    if (pending || anchor === undefined) {
      return;
    }

    run({
      change: {
        kind: "column.moved",
        columnId: column.id,
        afterColumnId: anchor.afterColumnId,
      },
      endpoint: moveColumn(column.id, anchor),
      subject: "move this column",
    });
  }

  function handleDelete() {
    if (pending) {
      return;
    }

    run({
      change: { kind: "column.deleted", columnId: column.id },
      endpoint: deleteColumn(column.id),
      subject: "delete this column",
    });
  }

  const canMoveLeft = moveAnchor(columns, column.id, "left") !== undefined;
  const canMoveRight = moveAnchor(columns, column.id, "right") !== undefined;

  return (
    <div className={styles.tools}>
      <form className={styles.composer} noValidate onSubmit={handleRename}>
        <TextField
          disabled={pending}
          error={error}
          id={nameId}
          label="Column name"
          maxLength={maxLengthFor(MAX_NAME_CODE_POINTS)}
          onChange={setName}
          value={name}
        />

        <div className={styles.composerActions}>
          <button className={workspace.submit} disabled={pending} type="submit">
            {pending ? "Saving…" : "Save name"}
          </button>

          <button className={workspace.secondary} onClick={onClose} type="button">
            Cancel
          </button>
        </div>
      </form>

      <div className={styles.toolRow}>
        <button
          className={workspace.secondary}
          disabled={pending || !canMoveLeft}
          onClick={() => handleMove("left")}
          type="button"
        >
          ← Move left
        </button>

        <button
          className={workspace.secondary}
          disabled={pending || !canMoveRight}
          onClick={() => handleMove("right")}
          type="button"
        >
          Move right →
        </button>
      </div>

      {confirming ? (
        <ConfirmColumnDelete
          cardCount={cards.length}
          confirmRef={confirmRef}
          name={column.name}
          onCancel={() => setConfirming(false)}
          onConfirm={handleDelete}
          pending={pending}
        />
      ) : (
        <button
          className={workspace.danger}
          disabled={pending}
          onClick={() => setConfirming(true)}
          type="button"
        >
          Delete column
        </button>
      )}
    </div>
  );
}

/**
 * The confirmation, which names the number of cards that go with the column.
 *
 * Deleting a column deletes its cards. `columns` and `cards` share a composite
 * foreign key with `ON DELETE CASCADE`, so one `DELETE /columns/:id` removes
 * every card in it, and there is no unarchive and no undo — #49 covers the
 * absence, and this is the UI refusing to pretend otherwise.
 *
 * **The count is the whole point of this panel.** "Are you sure?" is a question
 * nobody reads. "Delete Doing and its 12 cards?" is a fact, and it is the fact
 * that changes the answer — the difference between a mis-made column and a
 * fortnight of work is exactly that number.
 *
 * It is a panel and not `window.confirm`, which cannot be styled, cannot be
 * tested, blocks the event loop, and is suppressible browser-wide.
 */
function ConfirmColumnDelete({
  name,
  cardCount,
  pending,
  onConfirm,
  onCancel,
  confirmRef,
}: {
  name: string;
  cardCount: number;
  pending: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  confirmRef: React.RefObject<HTMLButtonElement | null>;
}) {
  const cards = `${cardCount} ${cardCount === 1 ? "card" : "cards"}`;

  return (
    <div className={`${workspace.panel} ${workspace.panelDanger}`}>
      <h4 className={workspace.panelTitle}>
        {cardCount === 0
          ? `Delete ${name}?`
          : `Delete ${name} and its ${cards}?`}
      </h4>

      <div className={workspace.panelBody}>
        {cardCount === 0 ? (
          <p>
            This column is empty, so nothing is lost but the column itself.{" "}
            <strong>It cannot be undone</strong> — there is no way to restore a
            deleted column in this version.
          </p>
        ) : (
          <p>
            <strong>
              The {cards} in this column will be deleted with it, permanently.
            </strong>{" "}
            There is no undo and no way to list deleted cards, so this is the
            last point at which those {cards} exist. Move them to another column
            first if any of them still matters.
          </p>
        )}
      </div>

      <div className={styles.composerActions}>
        <button
          className={workspace.danger}
          disabled={pending}
          onClick={onConfirm}
          ref={confirmRef}
          type="button"
        >
          {pending
            ? "Deleting…"
            : cardCount === 0
              ? "Delete column"
              : `Delete column and ${cards}`}
        </button>

        <button
          className={workspace.secondary}
          disabled={pending}
          onClick={onCancel}
          type="button"
        >
          Keep it
        </button>
      </div>
    </div>
  );
}
