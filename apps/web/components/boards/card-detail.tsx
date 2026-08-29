"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { type CardPatch, deleteCard, updateCard } from "@/lib/api/endpoints";
import type { Card, Member } from "@/lib/api/types";
import { assigneeName } from "@/lib/board/assignee";
import { dueFromInput, dueLabel, dueToInput, isOverdue, sameDue } from "@/lib/board/due";
import type { BoardChange } from "@/lib/board/mutations";
import { isPendingId } from "@/lib/board/mutations";
import { formatDateTime } from "@/lib/workspace/format";
import {
  MAX_DESCRIPTION_CODE_POINTS,
  MAX_NAME_CODE_POINTS,
  cardChanged,
  maxLengthFor,
  validateCardTitle,
  validateDescription,
  validateDueAt,
} from "@/lib/workspace/rules";
import { SelectField, TextAreaField, TextField } from "@/components/workspace/fields";
import { useBoardMutation } from "./board-mutation";
import styles from "./board.module.css";
import workspace from "@/components/workspace/workspace.module.css";

/**
 * One card, in full — and, when it is given the wiring to do so, editable.
 *
 * # Every field the API exposes, and nothing it does not
 *
 * `cardBody` in `apps/api/internal/api/cards.go` is `id`, `board_id`,
 * `column_id`, `title`, `description`, `assignee_id`, `due_at`, `created_at`,
 * `updated_at`. All nine are accounted for here, three of them by being
 * somewhere better than a row in a table:
 *
 * - **`title`** is the heading, **`description`** the body, **`assignee_id`**,
 *   **`due_at`**, **`created_at`** and **`updated_at`** the four facts
 *   underneath. The first two of those are also on the tile, because a board is
 *   scanned rather than opened — see `CardFace` in `card-drag.tsx`.
 * - **`column_id`** is shown as the column's *name*, resolved from the columns
 *   the page already has. An opaque uuid tells a user nothing; the column is
 *   the one piece of a card's state that is not visible from the card itself.
 * - **`board_id`** is the board you are looking at. Rendering it would be
 *   restating the page you are on.
 * - **`id`** is in the address bar, because the open card is a search
 *   parameter. Copying the URL is how you send someone this card.
 *
 * **There is no rank or index**, because there is no such field in any response
 * and there is not meant to be — ADR 0004 keeps it off the wire so no client
 * can come to depend on a number that renumbering changes. "Third in Doing" is
 * a fact about the column, and the column is what shows it.
 *
 * # Editing is opt-in, and the read-only render is the same one as before
 *
 * {@link CardDetailProps.editing} is optional. Without it this renders exactly
 * what it rendered before #64 — a heading, a description and two timestamps —
 * and mounts nothing that needs a router. With it, an *Edit* and a *Delete*
 * button appear, and each swaps in its form only when pressed.
 *
 * That is a real mode rather than a switch left in for convenience: the panel
 * is a plain presentational component that any tree can render, and only the
 * board hands it the ability to write. It is also why the read-only view keeps
 * the heading and the description as text — a title that was always an `<input>`
 * would make copying a card's name a selection problem and would put a form
 * control in the accessibility tree for people who only came to read.
 *
 * # Not a dialog
 *
 * A panel beside the board, not a modal. A modal would need a focus trap, an
 * escape handler and an inert background, and it would hide the board you
 * opened the card to see. Editing happens in place, which is why #63 chose a
 * panel in the first place.
 *
 * The `id="card"` is what the board's card links point their fragment at, so
 * that on a narrow screen — where this panel sits above the board rather than
 * beside it — following a card brings the panel into view with no JavaScript.
 */

/** What the panel needs in order to change the card rather than only show it. */
export type CardEditing = {
  /** The board's optimistic reducer setter. */
  applyChange: (change: BoardChange) => void;
  /** The board's failure reporter. */
  report: (message: string | null) => void;
};

export type CardDetailProps = {
  card: Card;
  /** The name of the column the card is in, or null when it is not shown. */
  columnName: string | null;
  /**
   * Everyone in the workspace, for the assignee picker and for naming the
   * current assignee. Null when `GET /members` did not load — the panel then
   * shows the card without offering to reassign it, rather than offering a
   * picker with nobody in it.
   */
  members: readonly Member[] | null;
  /** The reader's clock, or null before there is one. See `use-due-clock.ts`. */
  now: number | null;
  /** The board without this card open — where the close link goes. */
  closeHref: string;
  /** Omit to render the card read-only. */
  editing?: CardEditing;
};

export function CardDetail({
  card,
  columnName,
  members,
  now,
  closeHref,
  editing,
}: CardDetailProps) {
  const [mode, setMode] = useState<"read" | "edit" | "delete">("read");

  // A card the server has not acknowledged has no id to address, so there is
  // nothing to PATCH or DELETE yet. It cannot normally be opened — its tile is
  // not a link — but the mode is reachable if the card is confirmed while the
  // panel is open, so the controls are simply not offered.
  const editable = editing !== undefined && !isPendingId(card.id);

  // Opening a different card starts this component again from scratch, because
  // `BoardView` keys it on the card's id. That is React's own answer to
  // "reset every piece of state when a prop changes", and it is why there is no
  // effect here resetting `mode`: an effect would run a render late, so opening
  // a second card mid-edit would show its title inside the first card's
  // half-finished form for one frame.
  if (editable && mode === "edit") {
    return (
      <CardEditor
        card={card}
        closeHref={closeHref}
        editing={editing}
        members={members}
        onDone={() => setMode("read")}
      />
    );
  }

  const created = formatDateTime(card.createdAt);
  const updated = formatDateTime(card.updatedAt);
  const assignee = card.assigneeId === null ? null : assigneeName(members, card.assigneeId);
  const due = dueLabel(card.dueAt, now);
  const overdue = now !== null && isOverdue(card.dueAt, now);

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

        <dt className={styles.detailTerm}>Assignee</dt>
        <dd className={styles.detailValue}>
          {card.assigneeId === null
            ? "Nobody"
            : (assignee ??
              // Assigned, and this page cannot say to whom — a member list that
              // did not load, or one read before the member was added. The uuid
              // is deliberately not printed: it would name the person to nobody
              // and expose a user id to everybody.
              "Someone not in this workspace's member list")}
        </dd>

        <dt className={styles.detailTerm}>Due</dt>
        <dd className={styles.detailValue}>
          {due === null ? (
            "No due date"
          ) : (
            <time dateTime={card.dueAt ?? undefined}>
              {due}
              {overdue && (
                <span className={styles.detailOverdue}> — overdue</span>
              )}
            </time>
          )}
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

      {editable &&
        (mode === "delete" ? (
          <ConfirmCardDelete
            card={card}
            closeHref={closeHref}
            editing={editing}
            onCancel={() => setMode("read")}
          />
        ) : (
          <div className={styles.composerActions}>
            <button
              className={workspace.secondary}
              onClick={() => setMode("edit")}
              type="button"
            >
              Edit card
            </button>

            <button
              className={workspace.danger}
              onClick={() => setMode("delete")}
              type="button"
            >
              Delete card
            </button>
          </div>
        ))}
    </article>
  );
}

/**
 * The picker's options: everyone in the workspace, and nobody.
 *
 * The empty option is first and is not a placeholder — it is the value that
 * unassigns the card, which is the request `Optional[string]` exists on the API
 * side to make expressible. A picker that opened on the current assignee with
 * no way back to "nobody" would waste that, and there would be no other way to
 * take a card off somebody.
 *
 * The address is beside the name because display names are not unique and two
 * people called Alex are otherwise the same option twice.
 */
function assigneeOptions(
  members: readonly Member[],
): readonly { value: string; label: string }[] {
  return [
    { value: "", label: "Unassigned" },
    ...members.map((member) => ({
      value: member.userId,
      label: `${member.displayName} (${member.email})`,
    })),
  ];
}

/**
 * The card's title, description, assignee and due date, as a form.
 *
 * # Only what changed is sent
 *
 * `PATCH /cards/:id` leaves out what it is not given, so sending only the edited
 * field is not an optimisation — it is the difference between renaming a card
 * and overwriting a colleague's description with the copy this page loaded a
 * minute ago. The card body is whole in the response either way, so nothing here
 * has to merge.
 *
 * A form that changed nothing closes instead of submitting: the handler answers
 * 400 to a body mentioning none of the four fields, and "at least one of title,
 * description, assignee_id or due_at is required" is a strange thing to tell
 * somebody who pressed Save on a card they did not touch.
 *
 * # Unassigning is a value, not an omission
 *
 * The assignee and the due date reach nullable columns, and the API reads both
 * through `Optional[string]` so that **absent** ("leave it alone") and **null**
 * ("clear it") are different requests. That is the whole reason the picker has
 * an empty option and the date field can be emptied: a form that could only ever
 * *set* those fields would let a card be assigned by mistake and never handed
 * back. `CardPatch` in `lib/api/endpoints.ts` carries the same three states
 * across the wire, and `BoardChange` carries them into the optimistic board, so
 * the unassignment is drawn the moment it is asked for rather than a round trip
 * later.
 *
 * # The limits and the formats are the API's own
 *
 * 200 code points on the title and 10,000 on the description, from
 * `maxNameLength` and `maxDescriptionLength` in `crud.go`, counted with
 * `codePointLength` because the service counts with `utf8.RuneCountInString`
 * and `String.length` is neither bytes nor code points. The due date is
 * converted to RFC 3339 **with an offset** by `lib/board/due.ts`, because
 * `parseDueAt` requires one. All three are checked before the request rather
 * than after the 400, and none is stricter than the service — a client-side rule
 * the server would have accepted is a refusal with no authority behind it.
 *
 * An empty description is allowed and clears the field, because
 * `optionalText(..., allowEmpty: true)` says so on the other side. An empty
 * title is not, because it does not.
 */
function CardEditor({
  card,
  closeHref,
  editing,
  members,
  onDone,
}: {
  card: Card;
  closeHref: string;
  editing: CardEditing;
  members: readonly Member[] | null;
  onDone: () => void;
}) {
  const ids = useId();
  const titleId = `${ids}-title`;
  const descriptionId = `${ids}-description`;
  const assigneeId = `${ids}-assignee`;
  const dueId = `${ids}-due`;

  const [title, setTitle] = useState(card.title);
  const [description, setDescription] = useState(card.description);
  // "" is the empty option, which is what makes unassigning reachable. A select
  // whose value is `string | null` would need the null spelled as some sentinel
  // in the DOM anyway; "" is the sentinel the platform already has.
  const [assignee, setAssignee] = useState(card.assigneeId ?? "");
  // Read once, in the reader's own zone. This component only ever mounts in a
  // browser — it is behind an Edit button — so there is no server render here to
  // disagree with, which is why the panel needs `now` and this form does not.
  const [due, setDue] = useState(() => dueToInput(card.dueAt));
  const [titleError, setTitleError] = useState<string | undefined>(undefined);
  const [descriptionError, setDescriptionError] = useState<string | undefined>(undefined);
  const [dueError, setDueError] = useState<string | undefined>(undefined);

  const titleRef = useRef<HTMLInputElement | null>(null);
  const { run, pending } = useBoardMutation(editing.applyChange, editing.report);

  useEffect(() => {
    titleRef.current?.focus();
  }, []);

  function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending) {
      return;
    }

    const problems = {
      title: validateCardTitle(title),
      description: validateDescription(description),
      due: validateDueAt(due),
    };

    setTitleError(problems.title);
    setDescriptionError(problems.description);
    setDueError(problems.due);

    if (
      problems.title !== undefined ||
      problems.description !== undefined ||
      problems.due !== undefined
    ) {
      return;
    }

    const next = {
      title: title.trim(),
      description: description.trim(),
      assigneeId: assignee === "" ? null : assignee,
      dueAt: dueFromInput(due),
    };

    if (!cardChanged(card, next)) {
      onDone();

      return;
    }

    // Only the fields that actually differ, so an edit to one cannot overwrite
    // somebody else's edit to another. A key left out is "leave it alone"; a key
    // present and null is "clear it" — see `CardPatch`.
    const body: CardPatch = {};

    if (next.title !== card.title) {
      body.title = next.title;
    }

    if (next.description !== card.description) {
      body.description = next.description;
    }

    if (next.assigneeId !== card.assigneeId) {
      body.assigneeId = next.assigneeId;
    }

    // `sameDue` and not `!==`, because the control has minute granularity and
    // the stored value does not: comparing the strings would send a PATCH for a
    // card due at 17:00:30 every time somebody opened it and pressed Save.
    if (!sameDue(next.dueAt, card.dueAt)) {
      body.dueAt = next.dueAt;
    }

    run({
      change: { kind: "card.updated", cardId: card.id, ...body },
      endpoint: updateCard(card.id, body),
      subject: "save this card",
      onSuccess: onDone,
    });
  }

  return (
    <article aria-labelledby="card-title" className={styles.detail} id="card">
      <div className={styles.detailHeader}>
        <h2 className={styles.detailTitle} id="card-title">
          Edit card
        </h2>

        <Link className={styles.detailClose} href={closeHref}>
          Close
        </Link>
      </div>

      <form className={styles.composer} noValidate onSubmit={handleSubmit}>
        <TextField
          disabled={pending}
          error={titleError}
          id={titleId}
          inputRef={titleRef}
          label="Title"
          maxLength={maxLengthFor(MAX_NAME_CODE_POINTS)}
          onChange={setTitle}
          value={title}
        />

        <TextAreaField
          disabled={pending}
          error={descriptionError}
          id={descriptionId}
          label="Description"
          maxLength={maxLengthFor(MAX_DESCRIPTION_CODE_POINTS)}
          onChange={setDescription}
          optional
          rows={6}
          value={description}
        />

        {members === null ? (
          // No picker rather than an empty one. `GET /members` did not answer,
          // so the only options this form could offer are none — and a control
          // whose sole choice is "Unassigned" is a button that silently
          // unassigns the card.
          <p className={styles.detailEmpty}>
            The member list did not load, so the assignee cannot be changed here.
            Reload the board to try again — the rest of this form still works.
          </p>
        ) : (
          <SelectField
            disabled={pending}
            id={assigneeId}
            label="Assignee"
            onChange={setAssignee}
            options={assigneeOptions(members)}
            value={assignee}
          />
        )}

        <TextField
          disabled={pending}
          error={dueError}
          hint="In your own time zone. Clear the field to remove the due date."
          id={dueId}
          label="Due"
          onChange={setDue}
          optional
          type="datetime-local"
          value={due}
        />

        <div className={styles.composerActions}>
          <button className={workspace.submit} disabled={pending} type="submit">
            {pending ? "Saving…" : "Save card"}
          </button>

          <button
            className={workspace.secondary}
            disabled={pending}
            onClick={onDone}
            type="button"
          >
            Cancel
          </button>
        </div>
      </form>
    </article>
  );
}

/**
 * Deleting a card, confirmed.
 *
 * `DELETE /cards/:id` is final — there is no unarchive for a card any more than
 * for a project (#49) — so the panel says so before it happens rather than
 * offering an undo it does not have.
 *
 * On success the panel navigates back to the board without `?card=`. The card
 * is gone, and leaving the URL pointing at it would render the "card not found"
 * state, which is a true sentence and a confusing one immediately after you
 * deleted the thing yourself.
 */
function ConfirmCardDelete({
  card,
  closeHref,
  editing,
  onCancel,
}: {
  card: Card;
  closeHref: string;
  editing: CardEditing;
  onCancel: () => void;
}) {
  const router = useRouter();
  const { run, pending } = useBoardMutation(editing.applyChange, editing.report);
  const confirmRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    confirmRef.current?.focus();
  }, []);

  function handleDelete() {
    if (pending) {
      return;
    }

    run({
      change: { kind: "card.deleted", cardId: card.id },
      endpoint: deleteCard(card.id),
      subject: "delete this card",
      onSuccess: () => router.push(closeHref),
    });
  }

  return (
    <div className={`${workspace.panel} ${workspace.panelDanger}`}>
      <h3 className={workspace.panelTitle}>Delete this card?</h3>

      <div className={workspace.panelBody}>
        <p>
          <strong>Deleting a card cannot be undone.</strong> There is no way to
          restore it and no way to list deleted cards, so its title and
          description go with it.
        </p>
      </div>

      <div className={styles.composerActions}>
        <button
          className={workspace.danger}
          disabled={pending}
          onClick={handleDelete}
          ref={confirmRef}
          type="button"
        >
          {pending ? "Deleting…" : "Delete card"}
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
