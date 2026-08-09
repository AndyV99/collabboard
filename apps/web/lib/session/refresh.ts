/**
 * Exchanging a refresh token for a new session, exactly once at a time.
 *
 * # Why single-flight is a correctness requirement here, not a nicety
 *
 * `apps/api` rotates refresh tokens and detects reuse. `SessionStore.Rotate`
 * mints a successor and leaves the old record in place *specifically* so that
 * presenting the old token again is recognisable as a replay — and
 * `Service.Refresh` responds to `ErrRefreshReused` by revoking the whole
 * session.
 *
 * So two concurrent refreshes with the same token do not merely waste a round
 * trip. The first rotates; the second is a replay; the session is destroyed and
 * the user is signed out with a perfectly good 14-day refresh token in their
 * cookie jar. A refresh stampede on this API is a self-inflicted logout.
 *
 * That is what the map below prevents: concurrent callers holding the same
 * refresh token share one in-flight request and one answer.
 *
 * # Sharing has to outlive the request, not just overlap it
 *
 * An in-flight map alone is not enough, and the end-to-end run for #59 proved
 * it: twelve concurrent requests produced one refresh *and* one reuse detection,
 * and half of them then failed. The stragglers were requests that arrived after
 * the shared attempt had already settled — so they found an empty map and spent
 * the same refresh token a second time, because the cookie *they* were carrying
 * was still the pre-rotation one. There is no ordering that fixes this: a
 * request in flight when the rotation happened cannot have the successor.
 *
 * So a settled outcome is remembered for {@link GRACE_MS} against the token that
 * was spent. A latecomer presenting an already-exchanged token gets the
 * successor this process already obtained instead of a replay.
 *
 * This is the same grace window a rotating-refresh-token implementation
 * conventionally puts on the server; doing it here needs no API change. The cost
 * is stated plainly: for those seconds, a stolen copy of a just-spent refresh
 * token would be answered from memory rather than reaching the API's reuse
 * detection. The window is short, bounded, and per-process, and the alternative
 * is a session that dies whenever a page issues two requests at once.
 *
 * # What none of it prevents
 *
 * Both the map and the window are per process. Two Next tasks behind a load
 * balancer, each handed a request from the same browser carrying the same
 * expired session, can still race, and the API's reuse detection will revoke the
 * session. Nothing in this file can fix that — it is coordination, not caching —
 * and it is filed as issue #69, which weighs a shared store here against a
 * rotation grace window on the API.
 *
 * It is also visible in `next dev`, where Turbopack evaluates the module graph
 * more than once so this is not a singleton: the same burst that produces one
 * refresh and no reuse detections against the production build produces two and
 * one against the dev server. That is a dev-server artefact rather than a
 * property of the deployed image, and #69 covers it too.
 */

import { apiBaseUrl } from "@/lib/api";
import { API_V1_PREFIX, sendRequest } from "@/lib/api/http";
import { type SessionTokens, parseSessionTokens } from "@/lib/api/types";
import { logEvent } from "@/lib/log";

/**
 * The outcome of a refresh.
 *
 * Three cases, because two of them are the same to a caller and must not be:
 * `rejected` means the session is over and the cookies should be cleared, while
 * `unavailable` means the API could not answer and the session may well still
 * be fine. Clearing cookies on `unavailable` would sign every user out during a
 * brief API blip.
 */
export type RefreshOutcome =
  | { status: "refreshed"; tokens: SessionTokens }
  | { status: "rejected"; reason: string }
  | { status: "unavailable"; reason: string };

/** Injectable seams, for tests. Production supplies none of them. */
export type RefreshDeps = {
  fetchImpl?: typeof fetch;
  baseUrl?: string;
};

/**
 * In-flight refreshes, keyed by the refresh token being spent.
 *
 * Keyed by the token rather than by "there is a refresh happening" so that two
 * different sessions — two users on one server, or one user in two
 * organizations — never wait on each other's request or, worse, receive each
 * other's tokens.
 *
 * The token is already in this process's memory (it arrived in a cookie header);
 * using it as a map key adds no exposure. Entries are removed as soon as the
 * request settles, so the map is empty at rest.
 */
const inFlight = new Map<string, Promise<RefreshOutcome>>();

/**
 * How long a settled outcome answers for the token that was spent.
 *
 * Long enough to cover a burst of requests a page issues together plus the
 * slowest of them still being in flight; short enough that it is not a second
 * session store. Seconds, not minutes, and deliberately not configurable — a
 * knob here is a knob that gets turned up.
 */
export const GRACE_MS = 10_000;

/** Upper bound on remembered outcomes, so a busy process cannot grow unboundedly. */
const GRACE_MAX_ENTRIES = 1000;

type GraceEntry = { outcome: RefreshOutcome; expiresAt: number };

const recent = new Map<string, GraceEntry>();

/** Test-only reset, so one test's state cannot leak into the next. */
export function __resetInFlightForTests(): void {
  inFlight.clear();
  recent.clear();
}

/** How many refreshes are currently in flight. Used by tests and nothing else. */
export function inFlightCount(): number {
  return inFlight.size;
}

/** How many settled outcomes are being remembered. Tests only. */
export function graceCount(): number {
  return recent.size;
}

/**
 * Refreshes the session, joining an identical request already in progress or
 * reusing one that just finished.
 *
 * The returned promise never rejects: every failure is a {@link RefreshOutcome}.
 */
export function refreshSession(
  refreshToken: string,
  deps: RefreshDeps = {},
  now: number = Date.now(),
): Promise<RefreshOutcome> {
  const existing = inFlight.get(refreshToken);

  if (existing !== undefined) {
    return existing;
  }

  const remembered = recent.get(refreshToken);

  if (remembered !== undefined) {
    if (remembered.expiresAt > now) {
      return Promise.resolve(remembered.outcome);
    }

    recent.delete(refreshToken);
  }

  // The entry is registered synchronously, before any await, so a second caller
  // arriving in the same tick of the event loop finds it. Registering it after
  // an await is the classic way to write a single-flight that is not one.
  const attempt = performRefresh(refreshToken, deps).then(
    (outcome) => {
      // Recorded before the in-flight entry is dropped, so there is no instant
      // in which a latecomer finds neither and starts a replay.
      remember(refreshToken, outcome, now);
      inFlight.delete(refreshToken);

      return outcome;
    },
    (error: unknown) => {
      // performRefresh folds every failure into an outcome, so this is a bug
      // rather than a network error. The slot still has to be released, or the
      // session can never refresh again in this process.
      inFlight.delete(refreshToken);

      throw error;
    },
  );

  inFlight.set(refreshToken, attempt);

  return attempt;
}

/**
 * Remembers a settled outcome against the token that was spent.
 *
 * `unavailable` is deliberately not remembered: the token was not spent, the
 * failure is transient, and the next caller should try again rather than be
 * told for ten seconds that the API is down.
 */
function remember(refreshToken: string, outcome: RefreshOutcome, now: number): void {
  if (outcome.status === "unavailable") {
    return;
  }

  for (const [token, entry] of recent) {
    if (entry.expiresAt <= now) {
      recent.delete(token);
    }
  }

  // Map preserves insertion order, so the first key is the oldest.
  while (recent.size >= GRACE_MAX_ENTRIES) {
    const oldest = recent.keys().next();

    if (oldest.done === true) {
      break;
    }

    recent.delete(oldest.value);
  }

  recent.set(refreshToken, { outcome, expiresAt: now + GRACE_MS });
}

async function performRefresh(
  refreshToken: string,
  deps: RefreshDeps,
): Promise<RefreshOutcome> {
  let baseUrl: string;

  try {
    baseUrl = deps.baseUrl ?? `${apiBaseUrl()}${API_V1_PREFIX}`;
  } catch (error) {
    // A malformed API_URL. `instrumentation.ts` normally kills the process at
    // boot for this, so reaching it means the boot check was bypassed. It is
    // not the user's session's fault, so it is `unavailable`, not `rejected`.
    return {
      status: "unavailable",
      reason: error instanceof Error ? error.message : String(error),
    };
  }

  const result = await sendRequest(
    {
      method: "POST",
      path: "/auth/refresh",
      body: { refresh_token: refreshToken },
      parse: parseSessionTokens,
    },
    { baseUrl, fetchImpl: deps.fetchImpl },
  );

  if (result.ok) {
    logEvent("info", "session refreshed", { event: "web.session.refreshed" });

    return { status: "refreshed", tokens: result.data };
  }

  // 401 is the API saying this token is unknown, already used, or belongs to a
  // membership that no longer exists — `writeAuthError` collapses all three
  // into one status and one message on purpose. All three mean the same thing
  // here: this session is over.
  //
  // 403 joins it: `ErrNoOrganization`/`ErrNotAMember` mean the subject can no
  // longer act anywhere, and retrying will never change that.
  const terminal =
    result.error.kind === "unauthorized" || result.error.kind === "forbidden";

  logEvent(terminal ? "info" : "warn", "session refresh failed", {
    event: "web.session.refresh_failed",
    kind: result.error.kind,
    http_status: result.error.status,
    terminal,
  });

  return terminal
    ? { status: "rejected", reason: result.error.kind }
    : { status: "unavailable", reason: result.error.kind };
}
