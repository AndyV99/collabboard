/**
 * The browser's realtime connection: open, subscribe, apply, reconnect.
 *
 * Every dependency that would make this untestable is an argument — `fetch`,
 * the clock, the timer, the jitter, and the token refresh. There is no
 * `setTimeout` and no `Math.random` in the body of this file. That is not
 * ceremony: the behaviours worth testing here are *a reconnect happened after
 * the right delay* and *this close code did not retry*, and both are races
 * rather than assertions if the schedule is real.
 *
 * # The one rule this class exists to enforce
 *
 * **Every `subscribed` frame is followed by a full re-fetch of the board.**
 * ADR 0005 is explicit that Redis pub/sub is at-most-once and holds nothing, so
 * anything published while this client was between connections is simply gone.
 * The re-fetch is not a safety net bolted onto the design — it *is* the
 * recovery design, and the reason the ADR could refuse a transactional outbox.
 *
 * It is therefore wired to the `subscribed` frame rather than to "we
 * reconnected", because those are not the same moment: a socket that opens and
 * is then refused the board never subscribes, and re-fetching for it would be a
 * read nobody can use. `onResubscribed` fires on the first subscribe as well as
 * every later one — the first render's props came from the server, but they
 * were read before the socket existed, so the same gap applies.
 */

import {
  type Recovery,
  type StreamClose,
  reconnectDelayMs,
  recoveryFor,
} from "./recovery";
import { type ServerFrame, parseFrame } from "./protocol";
import { type StreamMessage, boardStreamPath } from "./stream";

/** What the user is told about the connection. */
export type LiveStatus =
  /** No stream yet, or one being opened. Nothing is stale that was not already. */
  | { state: "connecting" }
  /** Subscribed. Changes from other people are arriving. */
  | { state: "live" }
  /** The stream dropped and will be retried. The board is stale meanwhile. */
  | { state: "reconnecting"; attempt: number }
  /** Not retrying. `notice` says why, and is meant to be shown. */
  | { state: "stopped"; notice: string };

export type BoardStreamHandlers = {
  /** A frame arrived. Called for every frame, in order. */
  onFrame: (frame: ServerFrame) => void;
  /**
   * The board is (re)subscribed and must be re-read. See the note above: this
   * fires on the first subscribe too.
   */
  onResubscribed: () => void;
  onStatus: (status: LiveStatus) => void;
};

export type BoardStreamDeps = {
  fetchImpl: typeof fetch;
  setTimer: (run: () => void, ms: number) => number;
  clearTimer: (handle: number) => void;
  random: () => number;
  /**
   * Renews the access token. Resolves true when the session survived.
   *
   * Injected rather than called directly so a test can assert that a `4001`
   * refreshes *before* reconnecting, which is the whole difference between
   * recovering from an expired token and looping on one.
   */
  refreshSession: () => Promise<boolean>;
};

/** How many consecutive failures before the status admits it to the user. */
const QUIET_RETRIES = 1;

/**
 * A stream to one board.
 *
 * One instance per mounted board screen. `stop()` is final; a new board means a
 * new instance, which is what makes "events for a board the user is not viewing
 * are never applied" structural rather than a filter someone has to remember.
 */
export class BoardStream {
  readonly #boardId: string;
  readonly #handlers: BoardStreamHandlers;
  readonly #deps: BoardStreamDeps;

  #abort: AbortController | null = null;
  #timer: number | null = null;
  #attempt = 0;
  #stopped = false;
  /** Guards against two refreshes for two 4001s that arrive back to back. */
  #refreshing = false;

  constructor(boardId: string, handlers: BoardStreamHandlers, deps: BoardStreamDeps) {
    this.#boardId = boardId;
    this.#handlers = handlers;
    this.#deps = deps;
  }

  /** Opens the first stream. Safe to call once. */
  start(): void {
    if (this.#stopped) {
      return;
    }

    this.#handlers.onStatus({ state: "connecting" });
    void this.#connect();
  }

  /**
   * Closes the stream and cancels any pending retry.
   *
   * Idempotent, and permanent. Called from the hook's cleanup, so it runs on
   * unmount and on every dependency change React tears down for.
   */
  stop(): void {
    this.#stopped = true;

    if (this.#timer !== null) {
      this.#deps.clearTimer(this.#timer);
      this.#timer = null;
    }

    this.#abort?.abort();
    this.#abort = null;
  }

  async #connect(): Promise<void> {
    if (this.#stopped) {
      return;
    }

    const controller = new AbortController();

    this.#abort = controller;

    let response: Response;

    try {
      response = await this.#deps.fetchImpl(boardStreamPath(this.#boardId), {
        signal: controller.signal,
        headers: { accept: "text/event-stream" },
        // The session cookies are what authenticate this. Stated rather than
        // relied on: it is already the same-origin default, and it is the
        // entire authentication story for this request.
        credentials: "same-origin",
        cache: "no-store",
      });
    } catch {
      // Aborted by `stop()`, or the network refused. The first is not a
      // failure; the second is.
      if (!controller.signal.aborted) {
        this.#ended({ code: null, reason: "could not reach the server" });
      }

      return;
    }

    if (response.status === 401) {
      // The cookie has gone or expired without the socket ever being opened.
      // One refresh, then retry; a second 401 after a successful refresh would
      // mean the session is genuinely over, and `refreshSession` reports that.
      const renewed = await this.#renew();

      this.#ended(
        renewed
          ? { code: null, reason: "session renewed" }
          : { code: null, reason: "signed out" },
        renewed ? undefined : signedOutRecovery(),
      );

      return;
    }

    if (!response.ok || response.body === null) {
      this.#ended({ code: null, reason: `stream refused (${response.status})` });

      return;
    }

    await this.#read(response.body, controller);
  }

  async #read(body: ReadableStream<Uint8Array>, controller: AbortController): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    try {
      for (;;) {
        const { done, value } = await reader.read();

        if (done) {
          break;
        }

        buffer += decoder.decode(value, { stream: true });

        // SSE separates messages with a blank line. A partial one stays in the
        // buffer: a chunk boundary can fall anywhere, and a client that parsed
        // per chunk would drop whichever frame happened to be split.
        let split = buffer.indexOf("\n\n");

        while (split !== -1) {
          const block = buffer.slice(0, split);

          buffer = buffer.slice(split + 2);

          const message = decodeBlock(block);

          if (message !== null && this.#dispatch(message)) {
            return;
          }

          split = buffer.indexOf("\n\n");
        }
      }
    } catch {
      // A read error is a dropped connection. `stop()` aborts, which lands here
      // too, and is not a failure.
    } finally {
      try {
        reader.releaseLock();
      } catch {
        // Already released by the cancel above.
      }
    }

    if (!controller.signal.aborted && !this.#stopped) {
      // The body ended without a `closed` message. Treat it as a drop: the
      // relay always sends one, so this is the relay itself having died.
      this.#ended({ code: null, reason: "the stream ended" });
    }
  }

  /** Handles one decoded message. Returns true when the stream is finished. */
  #dispatch(message: StreamMessage): boolean {
    if (message.t === "open") {
      return false;
    }

    if (message.t === "closed") {
      this.#ended({ code: message.code, reason: message.reason });

      return true;
    }

    const frame = parseFrame(JSON.stringify(message.frame));

    if (frame === null) {
      return false;
    }

    this.#onFrame(frame);

    return false;
  }

  #onFrame(frame: ServerFrame): void {
    if (frame.kind === "shutdown") {
      // Recorded here rather than by the hook. When to reconnect is this
      // object's own business, and a hint that only worked because a caller
      // remembered to hand it back is a hint that stops working the first time
      // somebody writes a second caller.
      this.#serverHint = frame.reconnectAfterMs > 0 ? frame.reconnectAfterMs : null;
    }

    if (frame.kind === "subscribed") {
      if (frame.boardId !== this.#boardId) {
        // Cannot happen — the relay subscribes to one board and this instance
        // opened it — but a `subscribed` for another board would trigger a
        // re-fetch of *this* one, and being loud about impossible things is
        // cheaper than debugging them later.
        return;
      }

      this.#attempt = 0;
      this.#handlers.onStatus({ state: "live" });
      this.#handlers.onResubscribed();

      return;
    }

    if (frame.kind === "unsubscribed" && frame.reason === "forbidden") {
      // The 30-second re-authorisation sweep found the board no longer
      // resolves. After a `board.deleted` that is expected rather than an
      // authorisation failure, so the frame is passed on and the hook — which
      // knows whether it saw a delete — decides what to say.
      this.#handlers.onFrame(frame);

      return;
    }

    this.#handlers.onFrame(frame);
  }

  #ended(close: StreamClose, override?: Recovery): void {
    this.#abort = null;

    if (this.#stopped) {
      return;
    }

    const recovery = override ?? recoveryFor(close);

    if (recovery.action === "stop") {
      this.#stopped = true;
      this.#handlers.onStatus({
        state: "stopped",
        notice: recovery.notice ?? "Live updates have stopped.",
      });

      return;
    }

    if (recovery.escalates) {
      this.#attempt += 1;
    }

    // A single blip is not worth a banner. The first retry is silent and the
    // status stays as it was; anything more says so, because a board that has
    // quietly stopped updating is the failure #53 is about.
    if (this.#attempt > QUIET_RETRIES) {
      this.#handlers.onStatus({ state: "reconnecting", attempt: this.#attempt });
    }

    const delay = reconnectDelayMs({
      recovery,
      attempt: this.#attempt,
      serverHintMs: this.#serverHint,
      random: this.#deps.random,
    });

    this.#serverHint = null;

    this.#timer = this.#deps.setTimer(() => {
      this.#timer = null;

      if (recovery.action === "refresh-then-reconnect") {
        void this.#renew().then((renewed) => {
          if (renewed) {
            void this.#connect();

            return;
          }

          this.#stopped = true;
          this.#handlers.onStatus({
            state: "stopped",
            notice: signedOutRecovery().notice ?? "Live updates have stopped.",
          });
        });

        return;
      }

      void this.#connect();
    }, delay);
  }

  /** The `reconnect_after_ms` from a `shutdown` frame, used once. */
  #serverHint: number | null = null;

  /**
   * Drops the current stream and applies `recovery`, as if it had closed.
   *
   * For the refusals that arrive as *frames* rather than as close codes: the
   * socket is still perfectly healthy, but it is subscribed to nothing, which
   * to this screen is the same as being disconnected. `error`/`forbidden` and a
   * `unsubscribed`/`forbidden` from the re-authorisation sweep both land here,
   * with different recoveries, because one is "you never had access" and the
   * other is "you have just lost it".
   */
  abandon(recovery: Recovery): void {
    const controller = this.#abort;

    this.#abort = null;
    controller?.abort();

    this.#ended({ code: null, reason: "the subscription was refused" }, recovery);
  }

  async #renew(): Promise<boolean> {
    if (this.#refreshing) {
      return true;
    }

    this.#refreshing = true;

    try {
      return await this.#deps.refreshSession();
    } catch {
      return false;
    } finally {
      this.#refreshing = false;
    }
  }
}

/** The recovery for a session that is genuinely over. */
function signedOutRecovery(): Recovery {
  return {
    action: "stop",
    notice:
      "Your session ended, so this board has stopped updating. " +
      "Reload the page to sign in again.",
    escalates: false,
  };
}

/**
 * Decodes one SSE block into a message, or null for a comment or a stray line.
 *
 * Only `data:` is read. `event:` and `id:` are not used by this relay, and a
 * block that is only a heartbeat comment is exactly the case that must decode
 * to nothing rather than to a parse failure.
 */
export function decodeBlock(block: string): StreamMessage | null {
  const data = block
    .split("\n")
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).trimStart())
    .join("\n");

  if (data === "") {
    return null;
  }

  let value: unknown;

  try {
    value = JSON.parse(data);
  } catch {
    return null;
  }

  if (typeof value !== "object" || value === null) {
    return null;
  }

  const t = (value as { t?: unknown }).t;

  if (t === "open") {
    return { t: "open" };
  }

  if (t === "frame") {
    return { t: "frame", frame: (value as { frame?: unknown }).frame };
  }

  if (t === "closed") {
    const code = (value as { code?: unknown }).code;
    const reason = (value as { reason?: unknown }).reason;

    return {
      t: "closed",
      code: typeof code === "number" ? code : null,
      reason: typeof reason === "string" ? reason : "",
    };
  }

  return null;
}
