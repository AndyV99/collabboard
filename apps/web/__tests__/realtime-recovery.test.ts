/**
 * What each close code means, and how long to wait.
 *
 * Pure functions over a close, so the whole policy is a table. The behaviour
 * that *uses* this table is demonstrated in `realtime-client.test.ts`; this
 * file is about the decisions themselves, including the ones a client test
 * would find awkward to provoke.
 */

import { describe, expect, it } from "vitest";

import {
  BACKOFF_CEILING_MS,
  BACKOFF_FLOOR_MS,
  CLOSE_ABNORMAL,
  CLOSE_GOING_AWAY,
  CLOSE_MEMBERSHIP_REVOKED,
  CLOSE_MESSAGE_TOO_BIG,
  CLOSE_NORMAL,
  CLOSE_SLOW_CONSUMER,
  CLOSE_TOKEN_EXPIRED,
  CLOSE_UNSUPPORTED_DATA,
  backoffDelayMs,
  reconnectDelayMs,
  recoveryFor,
} from "@/lib/realtime/recovery";

describe("what a close code means", () => {
  it("refreshes rather than signs out when the token expired", () => {
    // 4001 is a fifteen-minute credential ending on schedule. Treating it as a
    // session ending would log out anyone who read a board for a quarter of an
    // hour.
    expect(recoveryFor({ code: CLOSE_TOKEN_EXPIRED, reason: "" })).toMatchObject({
      action: "refresh-then-reconnect",
    });
  });

  it("reconnects after being dropped as a slow consumer", () => {
    expect(recoveryFor({ code: CLOSE_SLOW_CONSUMER, reason: "" })).toMatchObject({
      action: "reconnect",
      escalates: true,
    });
  });

  it("stops, and says something the user can act on, when membership is revoked", () => {
    const recovery = recoveryFor({ code: CLOSE_MEMBERSHIP_REVOKED, reason: "" });

    expect(recovery.action).toBe("stop");
    // The notice has to say the board is stale, not just that something broke:
    // a board that has stopped updating looks exactly like one nobody is
    // editing, which is the failure #53 is about.
    expect(recovery.notice).toContain("out of date");
  });

  it("stops when the server rejected something this client sent", () => {
    // 1003 and 1009 are bugs here, not conditions. Retrying would send the same
    // thing again.
    for (const code of [CLOSE_UNSUPPORTED_DATA, CLOSE_MESSAGE_TOO_BIG]) {
      expect(recoveryFor({ code, reason: "" }).action).toBe("stop");
    }
  });

  it("reconnects without escalating when the instance is going away", () => {
    // A restart and a missed pong are both ordinary. Neither should make the
    // next genuine failure wait longer.
    expect(recoveryFor({ code: CLOSE_GOING_AWAY, reason: "" })).toMatchObject({
      action: "reconnect",
      escalates: false,
    });
  });

  it("reconnects and escalates for anything it has not been taught", () => {
    for (const code of [CLOSE_NORMAL, CLOSE_ABNORMAL, null, 4999]) {
      expect(recoveryFor({ code, reason: "" })).toMatchObject({
        action: "reconnect",
        escalates: true,
      });
    }
  });
});

describe("how long to wait", () => {
  it("grows exponentially and stops at the ceiling", () => {
    const full = (attempt: number) => backoffDelayMs(attempt, () => 1);

    expect(full(0)).toBe(500);
    expect(full(1)).toBe(1000);
    expect(full(2)).toBe(2000);
    expect(full(20)).toBe(BACKOFF_CEILING_MS);
  });

  it("never returns a delay short enough to be a hot loop", () => {
    // Full jitter is uniform over [0, window], so it does return single-digit
    // milliseconds — and a retry that comes back in 3 ms against an endpoint
    // that fails immediately is the self-inflicted denial of service the
    // backoff exists to prevent. This floor was added after that loop exhausted
    // a test worker for real.
    for (let attempt = 0; attempt < 10; attempt += 1) {
      expect(backoffDelayMs(attempt, () => 0)).toBe(BACKOFF_FLOOR_MS);
      expect(backoffDelayMs(attempt, () => 0.000001)).toBeGreaterThanOrEqual(
        BACKOFF_FLOOR_MS,
      );
    }
  });

  it("spreads the herd rather than returning one delay", () => {
    // Without jitter every client that was connected to a downed instance comes
    // back at the same instant and knocks it over again.
    const delays = new Set(
      [0.1, 0.3, 0.5, 0.7, 0.9].map((value) => backoffDelayMs(4, () => value)),
    );

    expect(delays.size).toBe(5);
  });

  it("honours the server's hint over its own schedule", () => {
    expect(
      reconnectDelayMs({
        recovery: recoveryFor({ code: CLOSE_GOING_AWAY, reason: "" }),
        attempt: 7,
        serverHintMs: 1400,
        random: () => 1,
      }),
    ).toBe(1400);
  });

  it("comes back promptly after an expected end, whatever the attempt count", () => {
    // A token expiring is not a failure, so it must not inherit a long window
    // earned by earlier, unrelated failures.
    const delay = reconnectDelayMs({
      recovery: recoveryFor({ code: CLOSE_TOKEN_EXPIRED, reason: "" }),
      attempt: 9,
      serverHintMs: null,
      random: () => 1,
    });

    expect(delay).toBe(500);
  });

  it("uses the escalated window for a failure that keeps happening", () => {
    const delay = reconnectDelayMs({
      recovery: recoveryFor({ code: null, reason: "" }),
      attempt: 3,
      serverHintMs: null,
      random: () => 1,
    });

    expect(delay).toBe(4000);
  });
});
