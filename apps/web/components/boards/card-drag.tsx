"use client";

import {
  DndContext,
  DragOverlay,
  MouseSensor,
  TouchSensor,
  closestCorners,
  useDroppable,
  useSensor,
  useSensors,
} from "@dnd-kit/core";
import type { DragEndEvent, DragOverEvent, DragStartEvent, Over } from "@dnd-kit/core";
import { SortableContext, useSortable, verticalListSortingStrategy } from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import Link from "next/link";
import { useEffect, useRef } from "react";
import type { ReactNode } from "react";

import type { Card, Member } from "@/lib/api/types";
import { assigneeName, initialsFor } from "@/lib/board/assignee";
import { dueLabel, isOverdue } from "@/lib/board/due";
import type { CardDirection, DropOver } from "@/lib/board/mutations";
import { isPendingId } from "@/lib/board/mutations";
import { cardHref } from "@/lib/workspace/routes";
import styles from "./board.module.css";

/**
 * The pointer half of #65: turning a gesture into a `DropOver` and a side.
 *
 * # Why a library, and why this one
 *
 * Measured, not assumed: `@dnd-kit/core` + `/sortable` + `/utilities` add
 * **48.8 kB raw / 16.0 kB gzipped** to this app's client JavaScript (224.4 kB →
 * 240.4 kB gzipped over every route), and only on the board route. That is the
 * price, and the board had almost none of it before #64.
 *
 * What it buys that a hand-rolled implementation does not:
 *
 * - **Touch.** HTML5 drag-and-drop — `draggable` plus `dragstart`/`drop` — is
 *   free and would have been enough for a mouse, but mobile browsers fire none
 *   of those events, so that version of this feature simply does not exist on a
 *   phone. `TouchSensor` costs one line here.
 * - **Auto-scroll inside nested scrollers.** This board is a horizontal
 *   scroller of columns that are each their own vertical scroller
 *   (`board.module.css`), so dragging to a card below the fold, or to a column
 *   off the right-hand edge, needs the container under the pointer to scroll.
 *   That is the genuinely fiddly part and it is not code worth writing twice.
 * - **It is the part that cannot be unit tested.** jsdom has no layout, so
 *   `getBoundingClientRect` is zeroes and no library's pointer path — or a
 *   hand-written one — can be driven in `vitest`. Code that has to be verified
 *   by hand in a browser is the code that should have other people's users
 *   behind it.
 *
 * Rejected: `react-beautiful-dnd`, deprecated by Atlassian.
 * `@hello-pangea/dnd` is its maintained fork and does everything here including
 * a keyboard mode, but it is 31 kB gzipped and its `onDragEnd` reports source
 * and destination **indices**, which is the one currency ADR 0004 refuses;
 * every one would have to be turned back into an anchor. `@dnd-kit/react` 0.5.0
 * is the actively developed successor to what is used here and is a month old,
 * but it is pre-1.0 with a changing API.
 *
 * Two risks worth naming rather than discovering later: `@dnd-kit/core` 6.3.1
 * was published in December 2024 and has had no release since, and it calls
 * `react-dom`'s `unstable_batchedUpdates`, which React 19.2.8 still exports —
 * checked, not assumed — but the `unstable_` prefix is a promise to nobody.
 *
 * # What this file deliberately does not do
 *
 * It does not decide where the card lands. `onDragOver` and `onDragEnd` report
 * *what the pointer is over* and *which side of it*; `lib/board/mutations.ts`
 * turns that into an anchor. So the drag has no opinion about ordering, the
 * ordering logic has no opinion about pointers, and the half that can be tested
 * is tested.
 *
 * It also has no `KeyboardSensor`. dnd-kit's is geometric — arrow keys move the
 * card by pixels and re-run collision detection — which on a board of
 * independently scrolling columns is unpredictable to use and untestable in
 * jsdom for the same reason the pointer path is. The keyboard path in
 * `board-view.tsx` walks the *list* instead, which is both easier to follow and
 * a real test.
 */

/** The id given to a column's drop zone, kept out of the uuid namespace. */
export function columnDropId(columnId: string): string {
  return `column:${columnId}`;
}

/** Reads a `DropOver` back out of whatever dnd-kit says the pointer is over. */
function dropOver(over: Over): DropOver {
  const id = String(over.id);

  return id.startsWith("column:") ? { column: id.slice("column:".length) } : { card: id };
}

/**
 * Whether the dragged card's centre has passed the centre of what it is over.
 *
 * Only consulted when a card first enters another column, where this file owns
 * the preview and so has to choose a side. Within a column the sortable
 * strategy is already drawing the gap and `cardDropTarget` matches it from the
 * indices instead — see its comment.
 */
function isPast(event: DragOverEvent | DragEndEvent): boolean {
  const dragged = event.active.rect.current.translated;
  const over = event.over?.rect;

  if (dragged == null || over == null) {
    return false;
  }

  return dragged.top + dragged.height / 2 > over.top + over.height / 2;
}

export type DragReport = {
  cardId: string;
  over: DropOver;
  past: boolean;
};

/**
 * The board, wrapped so its cards can be dragged.
 *
 * `MouseSensor` and `TouchSensor` rather than the combined `PointerSensor`,
 * because the two need different activation rules and the combined one can only
 * have one. A mouse drag starts after 8px of movement, so a click still opens
 * the card instead of being eaten as a one-pixel drag. A touch drag starts after
 * a 200ms press, so a finger dragged up the column scrolls it — the behaviour a
 * distance-based rule would break, on the device where scrolling matters most.
 *
 * dnd-kit's own announcements are silenced. It renders its own live region, and
 * a board with two of them announces pointer drags in one voice and keyboard
 * moves in another, differently worded. `board-view.tsx` owns the one that
 * speaks, so both input methods say the same sentence about the same board.
 */
export function CardDragArea({
  children,
  members,
  now,
  onCancel,
  onDrop,
  onOver,
  onStart,
  overlay,
}: {
  children: ReactNode;
  members: readonly Member[] | null;
  now: number | null;
  onStart: (cardId: string) => void;
  onOver: (report: DragReport) => void;
  onDrop: (report: DragReport | null) => void;
  onCancel: () => void;
  overlay: Card | null;
}) {
  const sensors = useSensors(
    useSensor(MouseSensor, { activationConstraint: { distance: 8 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 200, tolerance: 8 } }),
  );

  function report(event: DragOverEvent | DragEndEvent): DragReport | null {
    if (event.over === null) {
      return null;
    }

    return {
      cardId: String(event.active.id),
      over: dropOver(event.over),
      past: isPast(event),
    };
  }

  return (
    <DndContext
      accessibility={{
        announcements: {
          onDragStart: () => undefined,
          onDragOver: () => undefined,
          onDragEnd: () => undefined,
          onDragCancel: () => undefined,
        },
      }}
      collisionDetection={closestCorners}
      onDragCancel={onCancel}
      onDragEnd={(event: DragEndEvent) => onDrop(report(event))}
      onDragOver={(event: DragOverEvent) => {
        const over = report(event);

        if (over !== null) {
          onOver(over);
        }
      }}
      onDragStart={(event: DragStartEvent) => onStart(String(event.active.id))}
      sensors={sensors}
    >
      {children}

      {/*
       * A floating copy follows the pointer, and the card in the list is left
       * dimmed in place. Without the overlay dnd-kit translates the real
       * element, which cannot survive this board's cross-column preview: the
       * card is re-parented into another column mid-drag, and a transform
       * measured against its old parent then points somewhere meaningless.
       */}
      <DragOverlay>
        {overlay === null ? null : (
          // The same face as the tile it was lifted from, meta included. A copy
          // that dropped the assignee and the due date would be a different
          // height from the card it is standing in for, so picking a card up
          // would visibly resize it.
          <div className={`${styles.card} ${styles.cardDragging}`}>
            <CardFace card={overlay} members={members} now={now} />
          </div>
        )}
      </DragOverlay>
    </DndContext>
  );
}

/**
 * One column's cards as a drop zone.
 *
 * The zone is the whole list box, not each card, so that the empty space under
 * the last card is droppable — which is the only way to reach the bottom of a
 * column with the pointer, and the only way into an empty one. `cardDropTarget`
 * reads a drop on the zone itself as "append".
 *
 * Cards the server has not acknowledged are left out of `items`. Their ids were
 * invented by this client (`pending:`), so they cannot be dragged and cannot be
 * an anchor; `anchorAt` walks back past them for the same reason.
 */
export function CardDropZone({
  cards,
  children,
  columnId,
  labelledBy,
}: {
  cards: readonly Card[];
  children: ReactNode;
  columnId: string;
  labelledBy: string;
}) {
  const { setNodeRef, isOver } = useDroppable({ id: columnDropId(columnId) });
  const items = cards.filter((card) => !isPendingId(card.id)).map((card) => card.id);
  const over = isOver ? ` ${styles.zoneOver}` : "";

  return (
    <SortableContext items={items} strategy={verticalListSortingStrategy}>
      {cards.length === 0 ? (
        // The empty state carries the drop zone rather than sitting beside an
        // empty list, so a column with nothing in it is still a target — and
        // `.stack` stays the direct flex child that scrolls when it is not.
        <p className={`${styles.columnEmpty}${over}`} ref={setNodeRef}>
          No cards in this column.
        </p>
      ) : (
        <ol
          aria-labelledby={labelledBy}
          className={`${styles.stack}${over}`}
          ref={setNodeRef}
          tabIndex={0}
        >
          {children}
        </ol>
      )}
    </SortableContext>
  );
}

/**
 * One card: a grip that moves it, and a link that opens it.
 *
 * **Two controls, not one.** The card is a `<Link>` because opening a card is a
 * navigation — that is #63's reasoning and it still holds — and a link cannot
 * also be the thing a keyboard user presses to start a move, because Enter on a
 * link means "follow it". So the grip is a `<button>` beside it. The whole card
 * is still draggable by pointer, which is what a pointer user expects; the grip
 * is what makes the same move reachable without one.
 *
 * The grip carries the card's title in its accessible name for the reason the
 * column's Edit button does: a column of eight buttons all called "Move" is
 * eight identical rows in a screen reader's element list.
 */
export function DraggableCard({
  card,
  members,
  now,
  projectId,
  boardId,
  selected,
  lifted,
  instructionsId,
  onLift,
  onNudge,
  onDrop,
  onCancel,
}: {
  card: Card;
  /** Everyone in the workspace, or null when that list did not load. */
  members: readonly Member[] | null;
  /** The reader's clock, or null before there is one. */
  now: number | null;
  projectId: string;
  boardId: string;
  selected: boolean;
  lifted: boolean;
  instructionsId: string;
  onLift: () => void;
  onNudge: (direction: CardDirection) => void;
  onDrop: () => void;
  onCancel: () => void;
}) {
  // `attributes` is deliberately not spread anywhere. It is dnd-kit's wiring for
  // its own `KeyboardSensor`, which this board does not use, and it carries
  // `role="button"` — which on the `<li>` replaces the `listitem` role that is
  // how a screen reader says "3 of 7", the one fact a person moving a card most
  // needs. `listeners` is the pointer half and is all that is wanted.
  const { listeners, setNodeRef, setActivatorNodeRef, transform, transition, isDragging } =
    useSortable({ id: card.id });

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
  };

  const gripRef = useRef<HTMLButtonElement | null>(null);

  /**
   * Keeps the focus on the grip for as long as the card is held.
   *
   * Not defensive: moving a card into another column re-parents this `<li>`, so
   * React unmounts the grip and mounts a new one, and the focus goes to the
   * document. Every key after the one that crossed the column then lands
   * nowhere — the first real board this ran against had exactly that, and the
   * move was announced correctly and then quietly never sent.
   *
   * No dependency array, because the render that has to be repaired is the one
   * where nothing in this component's own props changed — only its parent did.
   * The guard makes it a no-op on every other render.
   */
  useEffect(() => {
    if (lifted && gripRef.current !== null && document.activeElement !== gripRef.current) {
      gripRef.current.focus();
    }
  });

  /**
   * Picking the card up and putting it down are **not** handled here.
   *
   * They are on `onClick`, because that is the event an activation produces —
   * and a keypress is only one of the things that produce it. NVDA and JAWS in
   * browse mode activate a button by calling `click()`; so do Dragon and Voice
   * Control on "click Move Alpha", and so does a VoiceOver or TalkBack
   * double-tap. Handling Enter here and calling `preventDefault` would swallow
   * the click the browser was about to synthesise and leave this button doing
   * nothing at all for every one of them — on touch with a screen reader
   * running, where the drag is unusable too, that is no way to move a card.
   *
   * So Enter and Space are deliberately left alone to become a click. This
   * handles only the keys that have no activation meaning.
   */
  function handleKeyDown(event: React.KeyboardEvent<HTMLButtonElement>) {
    if (!lifted) {
      return;
    }

    if (event.key === "Escape") {
      event.preventDefault();
      onCancel();

      return;
    }

    const direction: CardDirection | undefined = {
      ArrowUp: "up" as const,
      ArrowDown: "down" as const,
      ArrowLeft: "left" as const,
      ArrowRight: "right" as const,
    }[event.key];

    if (direction !== undefined) {
      // Or the column's own scroller answers the arrow key and the card the
      // user is moving scrolls out from under them.
      event.preventDefault();
      onNudge(direction);
    }
  }

  return (
    <li
      className={isDragging ? `${styles.cardRow} ${styles.cardRowDragging}` : styles.cardRow}
      ref={setNodeRef}
      style={style}
      {...listeners}
    >
      <button
        aria-describedby={instructionsId}
        aria-pressed={lifted}
        className={lifted ? `${styles.grip} ${styles.gripLifted}` : styles.grip}
        onBlur={(event) => {
          // Focus left the only control that can drop or cancel, so the move
          // has no way to be finished. Abandoning the proposal is the honest
          // reading of that; committing something the user walked away from is
          // not.
          //
          // Only when focus went *somewhere*. A null `relatedTarget` is this
          // grip being unmounted as the card moves column, which the effect
          // above is in the middle of repairing — cancelling on that would end
          // every move that crossed a column, in the act of making it.
          if (lifted && event.relatedTarget !== null) {
            onCancel();
          }
        }}
        // A toggle, which is what `aria-pressed` promises: pressed means the
        // arrow keys are moving this card, and activating it again puts the
        // card down. Reachable by click, by Enter or Space, and by whatever an
        // assistive technology does to press a button.
        onClick={() => (lifted ? onDrop() : onLift())}
        onKeyDown={handleKeyDown}
        ref={(node) => {
          gripRef.current = node;
          setActivatorNodeRef(node);
        }}
        type="button"
      >
        <span aria-hidden="true">⠿</span>
        <span className={styles.visuallyHidden}>Move {card.title}</span>
      </button>

      <Link
        aria-current={selected ? "true" : undefined}
        className={selected ? `${styles.card} ${styles.cardSelected}` : styles.card}
        // An anchor with an href is draggable by default, so a mouse drag
        // begun on the title would race the browser's own link drag — URL ghost
        // image and all — against the sensor listening on the row.
        draggable={false}
        href={cardHref(projectId, boardId, card.id)}
      >
        <CardFace card={card} members={members} now={now} />
      </Link>
    </li>
  );
}

/**
 * What a card tile says, wherever one is drawn.
 *
 * # The assignee and the due date are here rather than in the panel alone
 *
 * A board is a scanning surface. "Who has this" and "when is it due" are the
 * two questions asked of a column at a glance, and an answer that needs a click
 * is not an answer to either — which is what made #48's fields invisible in
 * practice even though the API had carried them since it was merged.
 *
 * Both are drawn small and only when they exist. A card with no assignee and no
 * due date renders exactly what it rendered before, which is most cards.
 *
 * # The initials are decoration and the name is the content
 *
 * Two letters in a circle is a convention, not information: initials collide,
 * and a screen reader announcing "DO" has told the listener nothing. So the
 * circle is `aria-hidden` and the full name sits beside it in visually-hidden
 * text, which is what actually reaches the link's accessible name — "Alpha,
 * assigned to Dana Okoro, due 31 Aug 2026, 17:00" is a card you can find by
 * name in an element list.
 *
 * # Overdue appears only once the reader's clock does
 *
 * `now` is null on the server and on the first client render, and
 * `components/boards/use-due-clock.ts` explains why that is not a loading
 * state. While it is null the date is shown in the fixed UTC form every other
 * timestamp in this app uses and **nothing claims the card is late** — a claim
 * made against the server's clock is a claim about a machine in Ireland, and it
 * would be re-rendered as its opposite a frame later.
 */
export function CardFace({
  card,
  members,
  now,
}: {
  card: Card;
  members: readonly Member[] | null;
  now: number | null;
}) {
  const name = card.assigneeId === null ? null : assigneeName(members, card.assigneeId);
  const due = dueLabel(card.dueAt, now);
  const overdue = now !== null && isOverdue(card.dueAt, now);

  return (
    <>
      <span className={styles.cardTitle}>{card.title}</span>

      {card.description !== "" && (
        <span className={styles.cardBody}>{card.description}</span>
      )}

      {(card.assigneeId !== null || due !== null) && (
        <span className={styles.cardMeta}>
          {card.assigneeId !== null && (
            <span className={styles.cardAvatar}>
              <span aria-hidden="true">{name === null ? "?" : initialsFor(name)}</span>

              <span className={styles.visuallyHidden}>
                {name === null
                  ? // Assigned to somebody this page cannot name — a stale
                    // member list, or one that did not load. Saying so beats
                    // printing the uuid, which would put a user id on screen
                    // and still tell the reader nothing.
                    "assigned to a member not in this list"
                  : `assigned to ${name}`}
              </span>
            </span>
          )}

          {due !== null && (
            <time
              className={
                overdue ? `${styles.cardDue} ${styles.cardDueOverdue}` : styles.cardDue
              }
              // The machine-readable half stays the instant the API sent,
              // offset and all, whatever zone the text beside it is in.
              dateTime={card.dueAt ?? undefined}
            >
              {overdue && <span className={styles.visuallyHidden}>overdue, </span>}
              Due {due}
            </time>
          )}
        </span>
      )}
    </>
  );
}

/**
 * A card the server has not acknowledged, drawn inert.
 *
 * #64's reasoning, unchanged: its id was invented by this client, so a link to
 * `?card=<that id>` opens a panel for a card that does not exist. It has no grip
 * either, for the same reason one place further on — the API would answer 400 to
 * a `card_id` that is not a uuid.
 */
export function PendingCard({ card }: { card: Card }) {
  return (
    <li className={styles.cardRow}>
      <span className={`${styles.card} ${styles.cardPending}`}>
        <span className={styles.cardTitle}>{card.title}</span>
        <span className={styles.cardPendingNote}>Adding…</span>
      </span>
    </li>
  );
}
