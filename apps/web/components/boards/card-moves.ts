"use client";

import { useRouter } from "next/navigation";
import { useCallback, useRef } from "react";

import { moveCard } from "@/lib/api/endpoints";
import type { ApiError } from "@/lib/api/errors";
import type { BoardChange, CardMove } from "@/lib/board/mutations";
import { useBoardMutation } from "./board-mutation";

/**
 * Sending card moves: one at a time per card, and what to do when the server
 * says the anchor is stale.
 *
 * # Two drags of the same card must not race
 *
 * This is the bug this file exists for, and it is not hypothetical — it is what
 * a person flicking a card up two places does. Two drags produce two requests,
 * and if both are in flight the server sees them in whatever order the network
 * delivered. ADR 0004 says what happens then: both transactions take the target
 * column's lock and update the same `cards` row, so the result is *last writer
 * wins*, at the granularity of one card. Correct, and the wrong answer — because
 * "last" means last to arrive, not last the user asked for. Two drags land in
 * the order the network chose, which is the order the user chose only by luck.
 *
 * There is nothing to fix on the server. Last-writer-wins is the right semantic
 * for two *clients*; it is this client that must not be two clients. So each
 * card carries a chain: a move waits for that card's previous move to be
 * answered before its own request goes out. Different cards are independent and
 * never wait for each other, which is the same disjointness the fractional ranks
 * buy at the database.
 *
 * **The wait costs nothing on screen.** `useBoardMutation` applies the
 * optimistic change *before* the gate, so the second drag draws immediately and
 * only its request is held. The board therefore looks exactly as it does with no
 * queue at all; the queue is visible only in the order the requests arrive.
 *
 * And the second move's anchor is right by construction, because it was computed
 * from a board that already had the first move applied to it — `useOptimistic`
 * replays every pending change over the latest server value. An anchor is a card
 * id, so it survives the first move landing; that is the property ADR 0004 chose
 * it for.
 *
 * # What a 409 does: put it back, re-read, and say so
 *
 * `POST /cards/:id/move` answers 409 when `after_card_id` is not a card in the
 * target column — someone else moved or deleted the card this drop named. The
 * optimistic move comes off the board on its own (nothing keeps it once the
 * transition ends) and the board re-reads, so the user is looking at the truth
 * before they try again.
 *
 * **It does not retry.** That was the tempting half-measure and it is wrong
 * here. The user's intent survives the anchor going stale — "put this card
 * third" is still a sensible wish — but expressing it again means picking a new
 * anchor from the refreshed board, and "the third slot" is an *index*: a claim
 * about a list rather than about a row. ADR 0004 refused indexes precisely
 * because the server cannot tell a deliberate placement from a stale one. An
 * automatic retry would have to reinvent one and would silently place the card
 * somewhere nobody asked for, which is worse than asking. So the card goes back,
 * the board refreshes, and the message says what happened and what to do — the
 * intent is handed back to the person who has it rather than guessed at.
 *
 * # A refusal cancels what was queued behind it
 *
 * The same argument, one step further, and it is the part that is easy to miss.
 * A queued move was computed against a board with its predecessor applied, so
 * once the predecessor is refused it is a claim about a state the server never
 * reached. Worse, it usually *carries* its predecessor: drag a card to Done and
 * then nudge it down within Done, and the second move names Done too — so
 * letting it through after the first was refused performs the very move the user
 * has just been told did not happen, and makes the message a lie.
 *
 * So a refusal resolves the chain false, and everything still waiting on it is
 * abandoned without a request and without a second message. One failure, one
 * explanation, and a board that then re-reads: the user is looking at the truth
 * and can say what they want again.
 */

/**
 * The failure text for a refused move.
 *
 * Only 409 is special-cased. `describeWriteFailure`'s general 409 sentence —
 * "This changed while you were working on it. Reload the page and try again." —
 * is true but asks for a reload this hook has already done, and does not say the
 * card is back where it started, which is the fact that decides whether the user
 * drags again or goes looking for their card.
 *
 * The API has two 409s on this route (`apps/api/internal/api/cards.go`): a stale
 * `after_card_id`, and a `column_id` naming a column on another board. Only the
 * first is reachable from this screen — every column in the picker is one of
 * this board's — so the copy names it rather than hedging over a case that would
 * be a bug in this file, not a race. The API's error envelope has no code field,
 * so telling them apart would mean matching on its prose, which is a coupling
 * worth avoiding for a branch that cannot happen.
 */
function describeMoveFailure(error: ApiError): string | undefined {
  if (error.kind !== "conflict") {
    return undefined;
  }

  return (
    "Someone else moved the card you dropped this one next to, so that position " +
    "no longer exists. This card is back where it started and the board has been " +
    "refreshed — drop it again where you want it."
  );
}

/** Sends card moves, in order, per card. */
export function useCardMoves(
  applyChange: (change: BoardChange) => void,
  report: (message: string | null) => void,
): (move: CardMove) => void {
  const router = useRouter();
  const { run } = useBoardMutation(applyChange, report);

  /**
   * The tail of each card's chain, by card id.
   *
   * A ref rather than state because nothing renders from it: it orders requests
   * and never appears on screen. Entries are removed as they settle, so this
   * holds only the cards with a move actually in flight — usually none.
   *
   * Each promise resolves **true if that move was accepted**. A false travels
   * down the rest of the chain, so a card's queued moves are abandoned the
   * moment one of them is refused — see below for why that is not merely
   * tidiness.
   */
  const inFlight = useRef(new Map<string, Promise<boolean>>());

  return useCallback(
    (move: CardMove) => {
      const previous = inFlight.current.get(move.cardId);

      let settle!: (accepted: boolean) => void;
      const mine = new Promise<boolean>((resolve) => {
        settle = resolve;
      });

      inFlight.current.set(move.cardId, mine);

      // Read by `onSettled`, which runs after whichever of the two callbacks
      // below fired. A move that was abandoned at the gate sets neither, and
      // false is the right answer for it too.
      let accepted = false;

      run({
        change: { kind: "card.moved", ...move },
        endpoint: moveCard(move.cardId, {
          columnId: move.columnId,
          afterCardId: move.afterCardId,
        }),
        subject: "move this card",
        gate: previous === undefined ? undefined : () => previous,
        onSuccess: () => {
          accepted = true;
        },
        onSettled: () => {
          settle(accepted);

          // Only if nobody queued behind this one, or the tail that is still
          // waiting would be dropped and the move after it would not be ordered.
          if (inFlight.current.get(move.cardId) === mine) {
            inFlight.current.delete(move.cardId);
          }
        },
        describe: describeMoveFailure,
        onFailure: (error) => {
          // A 409 means this board is out of date, so re-read it. Inside the
          // transition, like the success path's refresh: the card stays where
          // the user put it until the server's answer replaces it, so it moves
          // back at the same moment the explanation appears rather than
          // snapping back to a board that is also about to change.
          if (error.kind === "conflict") {
            router.refresh();
          }
        },
      });
    },
    [router, run],
  );
}
