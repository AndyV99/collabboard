"use client";

import { useState, useSyncExternalStore } from "react";

/**
 * How often the board re-reads the clock.
 *
 * A minute, because that is the granularity the due-date control collects and
 * the granularity `dueLabel` prints. Anything finer would re-render the board
 * to change nothing; anything coarser would leave a card sitting one tick past
 * its deadline still drawn as on time, which is exactly the moment the marker
 * exists for.
 */
export const DUE_CLOCK_INTERVAL_MS = 60_000;

/**
 * The reader's clock, or null until there is one.
 *
 * # Null is not "loading", it is "the server is rendering"
 *
 * `BoardView` is a Client Component, which still means it is **rendered on the
 * server first** and then hydrated. Reading `Date.now()` during that render
 * would put the server's clock into the HTML and the browser's into the
 * hydration pass, and React would find the two texts different — the classic
 * timestamp mismatch, and one that gets *worse* under load, because the gap
 * between the two renders is however long the response spent in flight.
 *
 * So this returns null on the server and on the first client render, which are
 * the two renders that have to agree. `lib/board/due.ts` states what null
 * renders instead.
 *
 * # Why `useSyncExternalStore` rather than an effect
 *
 * "A value that is deliberately different on the server from what it becomes in
 * the browser" is the exact problem this hook was added to React for, and it is
 * the one API that expresses it as a *contract* rather than as a sequence:
 * `getServerSnapshot` is what the server and the hydrating render both read,
 * and nothing else can be read until React has committed.
 *
 * The obvious alternative — `useState(null)` plus an effect that immediately
 * sets the clock — produces the same pixels and is a cascading render, which
 * `react-hooks/set-state-in-effect` correctly refuses. It is also a weaker
 * statement: the null-then-value ordering would be an emergent property of when
 * effects happen to run, rather than something the API guarantees.
 *
 * # Why a timer at all
 *
 * A board is left open. Without a tick, a card due in ten minutes would still
 * be drawn as on time an hour later — the board would be silently wrong about
 * the one fact a due date is for, until something else happened to re-render
 * it. One store per board is the cheapest possible version of being right: the
 * cards read the clock from a prop, so there is one timer and one notification
 * per minute however many cards are on screen.
 */
export function useDueClock(intervalMs: number = DUE_CLOCK_INTERVAL_MS): number | null {
  // Created once per board and never replaced. A module-level singleton would
  // be one timer for the whole app, which sounds tidier and is not: a second
  // board mounting would inherit a clock that is already ticking, so its first
  // render would not be null and the hydration guarantee above would hold only
  // for whichever board happened to mount first.
  const [clock] = useState(() => createDueClock(intervalMs));

  return useSyncExternalStore(clock.subscribe, clock.getSnapshot, serverSnapshot);
}

/** What the server renders, and what the hydrating render must match. */
function serverSnapshot(): null {
  return null;
}

type DueClock = {
  subscribe: (onChange: () => void) => () => void;
  getSnapshot: () => number | null;
};

/**
 * A clock that starts when something subscribes and stops when nothing is.
 *
 * `getSnapshot` returns a *cached* value rather than calling `Date.now()`:
 * React compares consecutive snapshots by identity to decide whether to
 * re-render, so a snapshot that changed on every call would re-render the board
 * in a loop.
 */
function createDueClock(intervalMs: number): DueClock {
  let current: number | null = null;
  let timer: ReturnType<typeof setInterval> | undefined;
  const listeners = new Set<() => void>();

  function announce(): void {
    for (const listener of listeners) {
      listener();
    }
  }

  return {
    subscribe(onChange) {
      listeners.add(onChange);

      if (timer === undefined) {
        // The clock exists from the moment somebody is listening, which is
        // after the first render has been committed — so this is where null
        // stops being the answer. React re-reads the snapshot immediately after
        // subscribing, so the board does not wait a whole minute for it.
        current = Date.now();

        timer = setInterval(() => {
          current = Date.now();
          announce();
        }, intervalMs);
      }

      return () => {
        listeners.delete(onChange);

        if (listeners.size === 0 && timer !== undefined) {
          clearInterval(timer);
          timer = undefined;
          // Back to "no clock", so a remount behaves like a fresh mount rather
          // than serving a reading from whenever this board was last open.
          current = null;
        }
      };
    },

    getSnapshot() {
      return current;
    },
  };
}
