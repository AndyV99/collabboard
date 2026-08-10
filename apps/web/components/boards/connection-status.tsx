"use client";

import type { LiveStatus } from "@/lib/realtime/client";
import styles from "./board.module.css";

/**
 * Whether this board is still hearing about other people's changes.
 *
 * #53 is the issue this answers: until now a client whose fan-out was
 * unavailable rendered a board that looked exactly like a correct one and
 * quietly went stale. The failure mode of a realtime feature is not an error
 * message, it is *silence*, and silence is indistinguishable from nobody else
 * editing. So the connection has a visible state.
 *
 * Note the split of responsibilities with #53, which stays open: this reports
 * **this client's connection**, which it knows about first-hand. It does not
 * report whether the server's Redis fan-out is healthy — a connected client
 * whose API cannot reach Redis still shows "Live" here, because the server does
 * not tell it otherwise. Making the server say so is #53's job.
 *
 * # Why "Live" is stated rather than only its absence
 *
 * A badge that appears only when something is wrong has to be noticed to work,
 * and the thing it would be competing with for attention is a board someone is
 * dragging cards around. Showing the good state too means the user has a
 * baseline: the word changes, in a place they have already seen it, rather than
 * something new appearing at the edge of the screen.
 */
export function ConnectionStatus({ status }: { status: LiveStatus }) {
  const described = describe(status);

  return (
    <div className={styles.connection}>
      {/*
       * `role="status"` is polite: a reconnect is worth knowing about but must
       * not interrupt someone mid-drag. The dot is decorative and the text is
       * the whole message, so a screen reader hears a sentence rather than a
       * colour.
       */}
      <span
        aria-hidden="true"
        className={`${styles.connectionDot} ${styles[described.tone]}`}
      />
      <span className={styles.connectionLabel} role="status">
        {described.label}
      </span>

      {described.detail !== null && (
        <span className={styles.connectionDetail}>{described.detail}</span>
      )}
    </div>
  );
}

type Described = {
  label: string;
  detail: string | null;
  tone: "connectionLive" | "connectionWaiting" | "connectionStopped";
};

function describe(status: LiveStatus): Described {
  switch (status.state) {
    case "live":
      return { label: "Live", detail: null, tone: "connectionLive" };

    case "connecting":
      return { label: "Connecting…", detail: null, tone: "connectionWaiting" };

    case "reconnecting":
      return {
        label: "Reconnecting…",
        // The board is not wrong, it is *old*, and those need different
        // reactions from a user: one means reload, the other means wait.
        detail: "Changes by other people are not appearing yet.",
        tone: "connectionWaiting",
      };

    case "stopped":
      return { label: "Not live", detail: status.notice, tone: "connectionStopped" };
  }
}
