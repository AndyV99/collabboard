"use client";

import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { isPendingId } from "@/lib/board/mutations";
import type { BoardSnapshot } from "@/lib/board/snapshot";
import { applyLiveLog } from "@/lib/realtime/apply";
import { BoardStream, type BoardStreamDeps, type LiveStatus } from "@/lib/realtime/client";
import type { RealtimeEvent, ServerFrame } from "@/lib/realtime/protocol";
import type { Recovery } from "@/lib/realtime/recovery";

/**
 * Live updates for the board on screen.
 *
 * # The three layers, and which one this hook owns
 *
 * ```
 * snapshot (prop)     the last full read by the Server Component   the truth
 *   + live log        this hook: events since that read            seconds old
 *     + useOptimistic board-view.tsx: this user's own edits        ms old
 * ```
 *
 * This hook owns the middle one. It returns `snapshot` with the log folded over
 * it, and `board-view.tsx` feeds *that* into `useOptimistic` — so the user's own
 * unconfirmed edit is applied last and therefore wins.
 *
 * **That ordering is the whole answer to the flicker question.** A `card.moved`
 * for a card this user is currently dragging lands underneath their own
 * optimistic move, which is replayed on top of it, so the card does not jump out
 * from under the pointer. When their move settles, `router.refresh()` brings the
 * server's version and last-writer-wins decides — which is the semantic ADR 0004
 * and ADR 0005 already chose, surfaced rather than fought.
 *
 * # Why there is a re-read at all, when the events are being applied
 *
 * Because the log has to be *finite*. An event applied to the live layer and
 * never retired means this client is maintaining its own copy of the board
 * indefinitely from an at-most-once stream — and the first dropped event makes
 * it permanently, silently wrong. So every event schedules a re-read, and a
 * re-read retires the events it is known to include.
 *
 * # Which events a new snapshot retires, and why that rule is safe
 *
 * ADR 0005 publishes **after the commit and before the HTTP response**. So an
 * event observed here at time *t* proves its write was durable before *t*, and
 * therefore that any read which *started* after *t* reflects it.
 *
 * Each log entry is stamped with the number of reads this hook had started when
 * it arrived. When a fresh snapshot appears, entries stamped lower than the
 * current count are dropped: a read has started since they arrived, so this
 * snapshot — or a later one already on its way — contains them. Entries that
 * arrived *during* the read are kept and replayed, which costs nothing because
 * {@link applyLiveEvent} is idempotent, and which is the case that would
 * otherwise lose somebody else's change to a refresh that overlapped it.
 *
 * The counter only advances for reads *this hook* starts. An own-write refresh
 * from `board-mutation.ts` therefore retires nothing, which is the conservative
 * direction: keeping an event too long replays a no-op, dropping one too early
 * loses it until the next read.
 *
 * # Creates are the one event that is not applied immediately
 *
 * A locally pending card carries a `pending:` id that `lib/board/mutations.ts`
 * made deliberately unmatchable to a server id — that is the point of the
 * prefix. So while this client has an unconfirmed create on screen, there is no
 * way to tell whether an inbound `card.created` *is* that card coming back or
 * somebody else's new one, and applying it would draw the same card twice.
 *
 * While anything is pending, creates are left to the re-read, which replaces the
 * placeholder and adds the real row in one step. With nothing pending — the
 * usual case, since the collision needs this user to be creating something at
 * the same moment — they apply immediately like everything else.
 */

/** How long inbound events are gathered before a re-read is asked for. */
const READ_COALESCE_MS = 120;

/** One event, and how many reads had been started when it arrived. */
type LiveEntry = { event: RealtimeEvent; readsStarted: number };

/**
 * The live layer, as one value.
 *
 * Kept together rather than as three `useState`s because the retirement rule
 * reads all of them at once and a partially-updated combination — new base,
 * old counter — would retire the wrong entries.
 */
type LiveState = {
  boardId: string;
  /** The snapshot the entries were last retired against. */
  base: BoardSnapshot;
  entries: LiveEntry[];
  /** Reads this hook has started. See the retirement rule above. */
  readsStarted: number;
};

function fresh(boardId: string, base: BoardSnapshot): LiveState {
  return { boardId, base, entries: [], readsStarted: 0 };
}

export type BoardLive = {
  /** The board with the live log applied. Feed this to `useOptimistic`. */
  board: BoardSnapshot;
  status: LiveStatus;
};

export type BoardLiveOptions = {
  snapshot: BoardSnapshot;
  boardId: string;
  /**
   * Whether the board currently shows anything the server has not acknowledged.
   *
   * A ref rather than a value because the answer is read from the *optimistic*
   * board, which is computed from this hook's own output — passing it in as a
   * prop would be a cycle. `board-view.tsx` writes it in an effect once the
   * optimistic board for the frame is known; it is only ever read
   * asynchronously, when a frame arrives, so being one commit behind is not a
   * correctness question. See the note on creates above.
   */
  pending: { current: boolean };
  /** Overridden only by tests, which supply a fake stream and a fake clock. */
  deps?: Partial<BoardStreamDeps>;
};

export function useBoardLive({
  snapshot,
  boardId,
  pending,
  deps,
}: BoardLiveOptions): BoardLive {
  const router = useRouter();

  const [stored, setLive] = useState<LiveState>(() => fresh(boardId, snapshot));
  const [status, setStatus] = useState<LiveStatus>({ state: "connecting" });

  const coalesceTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const seenBoardDeleted = useRef(false);

  /**
   * Retires the entries this snapshot is known to include.
   *
   * Done during render rather than in an effect: an effect runs *after* the
   * board has been painted from the new snapshot with the stale log still
   * folded over it, which is one frame showing an event twice — visible as a
   * flash on exactly the interaction this issue is about. Adjusting state
   * during render for a changed input is the pattern React documents for this,
   * and the `!==` guards are what keep it from looping.
   *
   * The counter and the base both live in this one piece of state rather than
   * in refs, so the calculation reads only values this render was given. A ref
   * read here would be reading something the next render has already changed.
   */
  let live = stored;

  if (stored.boardId !== boardId) {
    // A different board. Nothing about the old one's stream is relevant, and
    // the entries describe a board that is no longer on screen.
    live = fresh(boardId, snapshot);
    setLive(live);
  } else if (stored.base !== snapshot) {
    const kept = stored.entries.filter((entry) => entry.readsStarted >= stored.readsStarted);

    live =
      kept.length === stored.entries.length
        ? { ...stored, base: snapshot }
        : { ...stored, base: snapshot, entries: kept };

    setLive(live);
  }

  /**
   * The router, held so that {@link startRead} never has to change identity.
   *
   * This is not defensive tidying, it is a bug that was written and found. The
   * effect below opens the stream and lists its callbacks as dependencies, so a
   * callback that changes identity on every render tears the connection down
   * and opens a new one on every render — and because opening one sets the
   * status, which renders, that is an infinite loop that wedges the tab.
   *
   * `useRouter()` returns a stable object in Next, so the loop does not happen
   * in the browser. It happened immediately in a unit test, where the mock
   * returns a fresh object per call. Depending on a framework's identity
   * guarantee for something this consequential is the fragility worth removing:
   * the connection's lifetime is now tied to `boardId` and nothing else.
   */
  const routerRef = useRef(router);

  useEffect(() => {
    routerRef.current = router;
  }, [router]);

  const startRead = useCallback(() => {
    setLive((state) => ({ ...state, readsStarted: state.readsStarted + 1 }));
    routerRef.current.refresh();
  }, []);

  /**
   * Asks for a re-read, at most once per {@link READ_COALESCE_MS}.
   *
   * A burst of events — somebody dragging a card through four columns, or a
   * column being deleted with its cards — is one read rather than one per
   * event. Without this, a busy board would put every client into a re-render
   * loop against the Server Component, which is the amplification the
   * WebSocket exists to remove.
   *
   * The *leading* edge is deliberately not used: the events have already been
   * applied to the live layer, so the screen is correct while the timer runs
   * and the delay costs the user nothing.
   */
  const scheduleRead = useCallback(() => {
    if (coalesceTimer.current !== null) {
      return;
    }

    coalesceTimer.current = setTimeout(() => {
      coalesceTimer.current = null;
      startRead();
    }, READ_COALESCE_MS);
  }, [startRead]);

  const streamRef = useRef<BoardStream | null>(null);

  useEffect(() => {
    // The entries and the counter are reset during render, when `boardId`
    // changes — not here. An effect resetting them would run after the new
    // board had already been drawn with the old board's events folded over it.
    seenBoardDeleted.current = false;

    const onFrame = (frame: ServerFrame): void => {
      const stream = streamRef.current;

      if (frame.kind === "error") {
        stream?.abandon(recoveryForErrorFrame(frame.reason));

        return;
      }

      if (frame.kind === "unsubscribed") {
        if (frame.reason === "forbidden" && !seenBoardDeleted.current) {
          stream?.abandon({
            action: "stop",
            notice:
              "You no longer have access to this board, so it has stopped " +
              "updating. What is on screen may already be out of date.",
            escalates: false,
          });
        }

        return;
      }

      if (frame.kind !== "event") {
        return;
      }

      // The relay subscribes to one board and this hook opened it, so a frame
      // for another board is not reachable. It is checked anyway, because
      // "events for a board the user is not viewing must never be applied" is a
      // requirement worth holding at both ends rather than trusting a relay to
      // keep — and because the check is one comparison.
      if (frame.event.boardId !== boardId) {
        return;
      }

      const event = frame.event;

      if (event.type === "board.deleted") {
        seenBoardDeleted.current = true;
        startRead();

        return;
      }

      if (event.type === "board.updated") {
        // The board's name is rendered by the Server Component above this
        // client boundary, so there is nothing for the live layer to change and
        // a re-read is the only thing that shows it.
        scheduleRead();

        return;
      }

      const isCreate = event.type === "card.created" || event.type === "column.created";

      if (!(isCreate && pending.current)) {
        setLive((state) => ({
          ...state,
          entries: [...state.entries, { event, readsStarted: state.readsStarted }],
        }));
      }

      scheduleRead();
    };

    const stream = new BoardStream(
      boardId,
      {
        onFrame,
        onResubscribed: () => {
          // ADR 0005's recovery path. Everything published while this client
          // was not subscribed is gone and nothing will resend it, so the board
          // is read in full — and the log is dropped, because it describes a
          // board older than the read about to arrive.
          setLive((state) => ({ ...state, entries: [] }));
          startRead();
        },
        onStatus: setStatus,
      },
      {
        fetchImpl: deps?.fetchImpl ?? ((input, init) => fetch(input, init)),
        setTimer:
          deps?.setTimer ??
          ((run, ms) => setTimeout(run, ms) as unknown as number),
        clearTimer:
          deps?.clearTimer ?? ((handle) => clearTimeout(handle as unknown as number)),
        random: deps?.random ?? Math.random,
        refreshSession: deps?.refreshSession ?? refreshSession,
      },
    );

    streamRef.current = stream;
    stream.start();

    return () => {
      stream.stop();
      streamRef.current = null;

      if (coalesceTimer.current !== null) {
        clearTimeout(coalesceTimer.current);
        coalesceTimer.current = null;
      }
    };
    // `deps` is a test seam and is never changed after mount by real callers.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [boardId, scheduleRead, startRead]);

  // `live.entries` rather than `stored.entries`: on the render where a new
  // snapshot retires some of them, `stored` is still the pre-retirement value
  // and folding it would draw the retired events one more time.
  const entries = live.entries;

  const board = useMemo(
    () => applyLiveLog(snapshot, entries.map((entry) => entry.event)),
    [snapshot, entries],
  );

  return { board, status };
}

/**
 * Whether anything on the board is waiting for the server to acknowledge it.
 *
 * Only creates produce an id the client invented, so only creates can collide
 * with an inbound event — but the check is over both kinds because a column
 * being created is the case that would otherwise let a card land nowhere.
 */
export function hasPendingEntities(snapshot: BoardSnapshot): boolean {
  return snapshot.columns.some(
    (entry) =>
      isPendingId(entry.column.id) || entry.cards.some((card) => isPendingId(card.id)),
  );
}

/**
 * What an `error` frame means for the connection.
 *
 * `unavailable` is the only retryable one: the server could not reach Postgres
 * to authorise, or is draining. The rest are refusals a retry cannot change —
 * `forbidden` for a board in another tenant, and the two request errors, which
 * would be bugs in this client.
 */
function recoveryForErrorFrame(reason: string): Recovery {
  if (reason === "unavailable") {
    return { action: "reconnect", notice: null, escalates: true };
  }

  if (reason === "forbidden") {
    return {
      action: "stop",
      notice:
        "You do not have access to this board's live updates, so it will not " +
        "update on its own.",
      escalates: false,
    };
  }

  return {
    action: "stop",
    notice: "Live updates stopped because of a problem in this page. Reload to start them again.",
    escalates: false,
  };
}

/**
 * Renews the session through this app's own Route Handler.
 *
 * The browser holds no refresh token — it asks the origin that does. A 204 is a
 * renewed session, a 401 is a session that is genuinely over, and a 502 is the
 * API being unreachable, which is worth retrying rather than signing out for.
 */
async function refreshSession(): Promise<boolean> {
  try {
    const response = await fetch("/api/auth/refresh", {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
    });

    return response.status !== 401;
  } catch {
    return true;
  }
}
