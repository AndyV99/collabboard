"use client";

import { useRouter } from "next/navigation";
import { useCallback, useTransition } from "react";

import { api } from "@/lib/api/browser";
import type { ApiError } from "@/lib/api/errors";
import type { Endpoint } from "@/lib/api/http";
import type { BoardChange } from "@/lib/board/mutations";
import { describeWriteFailure } from "@/lib/workspace/outcomes";

/**
 * One board edit: applied on screen, sent, and then either confirmed by the
 * server or dropped.
 *
 * # Why the rollback has no code
 *
 * The optimistic change is handed to React's `useOptimistic` (the store lives
 * in `board-view.tsx`) from inside the transition this hook starts. React holds
 * such a value only while its transition is pending and **discards it when the
 * transition ends**, re-rendering from the prop the server last sent. So the
 * two outcomes need no branch:
 *
 * - **Success** — `router.refresh()` runs inside the transition, so the
 *   transition stays pending until the Server Component has re-rendered and a
 *   new prop has arrived. The optimistic value is dropped at the moment the
 *   real one replaces it, which is why the board does not flash back to its old
 *   state on the way to its new one.
 * - **Failure** — the function returns, the transition ends, the prop never
 *   changed, and the board is exactly as it was. There is no `undo` to write
 *   and, more to the point, none to forget to write.
 *
 * The alternative — `useState` plus a hand-written inverse for each of the
 * seven operations — is the shape that produces the bug this issue is most
 * worried about, because the inverse is only exercised on a path nobody hits
 * locally. `__tests__/board-mutations.test.tsx` covers the refusal of every
 * operation anyway: structural or not, "the server said no and the UI kept the
 * change" is the regression worth a test.
 *
 * # Failures are reported upward, not held here
 *
 * `report` writes into state owned by `BoardView`, and that is deliberate
 * rather than incidental. A destructive edit removes the very element the
 * control sits in — delete a column and the button that deleted it unmounts
 * with it — so a failure message held in this component would be destroyed by
 * the optimistic update and never seen. The one component guaranteed to outlive
 * every change is the board itself.
 */

/** What to run, and what to say if it fails. */
export type BoardMutationInput<T> = {
  /** The optimistic edit, applied before the request goes out. */
  change: BoardChange;
  /** The request. */
  endpoint: Endpoint<T>;
  /**
   * The action, as {@link describeWriteFailure} will finish the sentence:
   * "create the card", "rename this column".
   */
  subject: string;
  /** Ran on success, before the refresh. Clearing a form belongs here. */
  onSuccess?: (value: T) => void;
  /**
   * Ran on failure. Restoring input the caller optimistically cleared belongs
   * here — the text someone typed is the one thing a failed write must not
   * cost them.
   */
  onFailure?: (error: ApiError) => void;
  /**
   * A message for this failure, overriding {@link describeWriteFailure}.
   *
   * Returning undefined falls through to the shared wording, so an override
   * only has to cover the case it knows more about than the general one does.
   * #65 uses it for a 409 on a move, where the generic "this changed while you
   * were working on it" is true but does not say which part of the board
   * changed, or that the card is back where it started.
   */
  describe?: (error: ApiError) => string | undefined;
  /**
   * Held between applying the change and sending it.
   *
   * The optimistic edit is on screen the whole time this is pending, so a gate
   * costs feel nothing; what it buys is ordering. #65 chains a card's moves on
   * one of these so that two drags of the same card reach the server in the
   * order the user made them — see `card-moves.ts`.
   */
  gate?: () => Promise<unknown>;
  /**
   * Ran once the server has answered, before the refresh and whatever the
   * answer was. Releasing a {@link gate} belongs here.
   */
  onSettled?: () => void;
};

export type BoardMutation = {
  /** Applies the change, sends the request, and settles it. */
  run: <T>(input: BoardMutationInput<T>) => void;
  /** Whether this hook's own transition is in flight. */
  pending: boolean;
};

/**
 * Builds the runner.
 *
 * `applyChange` is the `useOptimistic` setter from `BoardView`. It is called
 * *inside* the transition started here, which is what ties the optimistic value
 * to this request's lifetime — React scopes an optimistic update to whichever
 * transition is active when the setter runs, not to the component that owns the
 * store, so the store can sit above the control that writes to it.
 *
 * `report` is `BoardView`'s failure setter. Both are passed in rather than read
 * from a context because there is exactly one board on the screen and a context
 * would be indirection with no second implementation to justify it.
 */
export function useBoardMutation(
  applyChange: (change: BoardChange) => void,
  report: (message: string | null) => void,
): BoardMutation {
  const router = useRouter();
  const [pending, startTransition] = useTransition();

  const run = useCallback(
    <T,>({
      change,
      endpoint,
      subject,
      onSuccess,
      onFailure,
      describe,
      gate,
      onSettled,
    }: BoardMutationInput<T>) => {
      startTransition(async () => {
        applyChange(change);

        try {
          // Before the request, after the paint. Whatever this waits for, the
          // optimistic change is already on screen — which is what makes
          // serialising a card's moves free at the interaction layer.
          await gate?.();

          const result = await api(endpoint);

          if (!result.ok) {
            // Both of these are `useState` setters, which React defers to the
            // end of the transition. That is the behaviour wanted rather than a
            // limitation worked around: the message appears at the same instant
            // the optimistic change disappears, so the board is never showing a
            // rejected edit and an explanation of it at the same time.
            report(
              describe?.(result.error) ?? describeWriteFailure(result.error, subject),
            );
            onFailure?.(result.error);

            return;
          }

          report(null);
          onSuccess?.(result.data);

          // Inside the transition on purpose. `router.refresh()` re-renders the
          // Server Component, and keeping the transition pending until that
          // lands is what holds the optimistic value on screen across the gap.
          //
          // It is also the whole of rule 3 in apps/web/README.md: a reorder is
          // decided by re-reading what the server returns, never by keeping the
          // array this client spliced. ADR 0004's ranks are not on the wire, so
          // there is nothing else the client could legitimately be ordering by.
          router.refresh();
        } finally {
          // In a `finally` because a gate is a *queue*: whoever is waiting on
          // this one is stuck for the life of the page if an unexpected throw
          // skips the release. `api` returns its failures as values rather than
          // throwing, so this should never be the reason it runs — which is
          // exactly why it would not be noticed if it were missing.
          onSettled?.();
        }
      });
    },
    [applyChange, report, router],
  );

  return { run, pending };
}
