/**
 * What to do when the stream ends — which is a different question for each way
 * it can end.
 *
 * The server has four app-level close codes and several protocol ones, and the
 * whole point of having more than one is that they do not mean the same thing
 * to a person. Collapsing them into "reconnect" would sign nobody out but would
 * leave a removed member's browser retrying forever against a socket that will
 * refuse it every time, and would leave an expired token reconnecting with the
 * same expired token in a tight loop. So the codes are decided here, once, as a
 * pure function over a close, and `use-board-live.ts` only carries it out.
 *
 * Everything in this file is pure. The jitter and the clock are arguments.
 */

/**
 * The application close codes (`internal/realtime/realtime.go:69-82`).
 *
 * Named rather than inlined because a bare `4003` in a `switch` is the kind of
 * thing that gets copied to the wrong branch.
 */
export const CLOSE_TOKEN_EXPIRED = 4001;
export const CLOSE_SLOW_CONSUMER = 4002;
export const CLOSE_MEMBERSHIP_REVOKED = 4003;

/** WebSocket protocol codes this client can meet. */
export const CLOSE_NORMAL = 1000;
export const CLOSE_GOING_AWAY = 1001;
export const CLOSE_UNSUPPORTED_DATA = 1003;
export const CLOSE_NO_STATUS = 1005;
export const CLOSE_ABNORMAL = 1006;
export const CLOSE_MESSAGE_TOO_BIG = 1009;

/** How the stream ended, as the browser client learns it. */
export type StreamClose = {
  /** The upstream WebSocket close code, or null when the SSE itself failed. */
  code: number | null;
  reason: string;
};

/**
 * What to do next.
 *
 * - `reconnect` — open a new stream after `delayMs`. The re-subscribe re-fetches
 *   the board, so nothing missed while the socket was down stays missed.
 * - `refresh-then-reconnect` — renew the access token first. Reconnecting
 *   without renewing would present the same expired credential and be closed
 *   again immediately, which is a loop rather than a recovery.
 * - `stop` — do not retry. Something a retry cannot fix.
 */
export type Recovery = {
  action: "reconnect" | "refresh-then-reconnect" | "stop";
  /** What to tell the user, or null when this is not worth interrupting for. */
  notice: string | null;
  /**
   * Whether this close should advance the backoff counter.
   *
   * A token expiring on schedule and a server restarting are *expected* ends,
   * not failures, so they reconnect promptly and do not make the next
   * legitimate failure wait longer. A refused connection does advance it.
   */
  escalates: boolean;
};

/**
 * The recovery for one close.
 *
 * `4001` deliberately does **not** sign the user out. An expired access token
 * is the normal end of a fifteen-minute credential on a page someone has left
 * open; the refresh cookie is still good, and treating a scheduled expiry as a
 * session ending would log out anybody who read a board for a quarter of an
 * hour.
 *
 * `4003` deliberately does **not** retry. Membership was revoked, so every
 * reconnect would be authorised, refused and closed again — a retry loop whose
 * only effect is load. The user is told, and the board they are looking at is
 * whatever was last read; it is stale from this moment and the notice says so.
 *
 * `4002` is the interesting one, because it is the case ADR 0005 designed the
 * re-fetch for. The server dropped this connection *because* it had fallen
 * behind, so events were missed by definition. Reconnecting re-subscribes,
 * re-subscribing re-fetches, and the gap closes. It backs off first so that a
 * client which is slow because the machine is busy does not immediately earn
 * the same fate.
 */
export function recoveryFor(close: StreamClose): Recovery {
  switch (close.code) {
    case CLOSE_TOKEN_EXPIRED:
      return {
        action: "refresh-then-reconnect",
        notice: null,
        escalates: false,
      };

    case CLOSE_SLOW_CONSUMER:
      return {
        action: "reconnect",
        notice: null,
        escalates: true,
      };

    case CLOSE_MEMBERSHIP_REVOKED:
      return {
        action: "stop",
        notice:
          "You no longer have access to this board, so it has stopped updating. " +
          "What is on screen may already be out of date.",
        escalates: false,
      };

    case CLOSE_UNSUPPORTED_DATA:
    case CLOSE_MESSAGE_TOO_BIG:
      // The server rejected something this client sent. Retrying would send it
      // again. This is a bug in the client rather than a condition, so it stops
      // and says the board is no longer live rather than pretending otherwise.
      return {
        action: "stop",
        notice:
          "Live updates stopped because of a problem in this page. " +
          "Reload to start them again.",
        escalates: false,
      };

    case CLOSE_GOING_AWAY:
      // The instance is restarting, or missed a pong. Both are ordinary and
      // both are fixed by connecting again — to a different instance, probably.
      return { action: "reconnect", notice: null, escalates: false };

    default:
      return { action: "reconnect", notice: null, escalates: true };
  }
}

/** Backoff bounds. Exported so the tests do not restate them. */
export const BACKOFF_BASE_MS = 500;
export const BACKOFF_CEILING_MS = 30_000;

/**
 * The shortest gap between two connection attempts, ever.
 *
 * Full jitter is uniform over `[0, window]`, so it can and does return
 * single-digit milliseconds — and a first retry with a 500 ms window that comes
 * back in 3 ms, against an endpoint that fails immediately, is a loop bounded
 * only by how fast the machine can dial. That is precisely the self-inflicted
 * denial of service the backoff exists to prevent, so the floor is not a
 * rounding detail: it is the guarantee.
 *
 * It was also observed for real. Before this existed, mounting a board in a
 * test environment with no realtime endpoint spun the reconnect loop hard
 * enough to exhaust a Vitest worker.
 */
export const BACKOFF_FLOOR_MS = 100;

/**
 * How long to wait before attempt number `attempt` (0 is the first retry).
 *
 * Exponential from half a second to thirty, with full jitter. The ceiling and
 * the jitter are both about the same failure: an API that is down has every
 * client that was connected to it trying to reconnect, and without a ceiling
 * they give up usefully slowly, while without jitter they all return at the
 * same instant and knock it over again the moment it comes back. `random` is an
 * argument so a test gets a schedule rather than a distribution.
 *
 * Full jitter — uniform over `[0, window]` rather than `window/2 + random` —
 * because the point is to spread the herd, and the cost of an occasional short
 * wait is one extra request.
 */
export function backoffDelayMs(attempt: number, random: () => number): number {
  const window = Math.min(BACKOFF_BASE_MS * 2 ** Math.max(0, attempt), BACKOFF_CEILING_MS);

  return Math.max(BACKOFF_FLOOR_MS, Math.round(window * random()));
}

/**
 * The delay before reconnecting after a close.
 *
 * A `shutdown` frame carries the server's own hint (`reconnect_after_ms`,
 * jittered server-side into `[hint, 2*hint)`), and honouring it is what keeps a
 * rolling deploy from becoming a thundering herd against the instance that is
 * still up. It wins over the local schedule when it is present.
 */
export function reconnectDelayMs(input: {
  recovery: Recovery;
  attempt: number;
  serverHintMs: number | null;
  random: () => number;
}): number {
  if (input.serverHintMs !== null && input.serverHintMs > 0) {
    return input.serverHintMs;
  }

  if (!input.recovery.escalates) {
    // An expected end. Come back quickly, but not instantly — an immediate
    // reconnect on every token expiry across a large room is still a spike.
    return backoffDelayMs(0, input.random);
  }

  return backoffDelayMs(input.attempt, input.random);
}
