/**
 * The single-flight refresh, which is the part of #59 that is a correctness
 * requirement rather than an optimisation.
 *
 * `apps/api` rotates refresh tokens and revokes the whole session when it sees
 * one presented twice (`SessionStore.Rotate` → `ErrRefreshReused` →
 * `Service.Refresh` revokes). So a refresh stampede is not a wasted round trip,
 * it is a self-inflicted logout. These tests assert the call count rather than
 * observing that it "looks fine".
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  __resetInFlightForTests,
  GRACE_MS,
  graceCount,
  inFlightCount,
  refreshSession,
} from "@/lib/session/refresh";

const BASE = "http://api.test/api/v1";

function session(accessToken: string, refreshToken: string) {
  return {
    token_type: "Bearer",
    access_token: accessToken,
    expires_in: 900,
    refresh_token: refreshToken,
    user_id: "11111111-1111-4111-8111-111111111111",
    organization: { id: "org-1", name: "Acme", slug: "acme", role: "owner" },
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** A fetch that does not settle until `release()` is called. */
function deferredFetch(response: () => Response) {
  const calls: string[] = [];
  let release!: () => void;
  const gate = new Promise<void>((resolve) => {
    release = resolve;
  });

  const fetchImpl = (async (url: string) => {
    calls.push(url);
    await gate;

    return response();
  }) as unknown as typeof fetch;

  return { fetchImpl, calls, release: () => release() };
}

beforeEach(() => {
  __resetInFlightForTests();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("refreshSession", () => {
  it("exchanges the refresh token for a new session", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse(session("new-access", "new-refresh")));

    const outcome = await refreshSession("old-refresh", {
      baseUrl: BASE,
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });

    expect(outcome.status).toBe("refreshed");
    expect(outcome.status === "refreshed" && outcome.tokens.accessToken).toBe(
      "new-access",
    );
    // The rotated token, which the caller must persist — the old one is now a
    // replay as far as the API is concerned.
    expect(outcome.status === "refreshed" && outcome.tokens.refreshToken).toBe(
      "new-refresh",
    );

    const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];

    expect(url).toBe(`${BASE}/auth/refresh`);
    expect(JSON.parse(init.body as string)).toEqual({ refresh_token: "old-refresh" });
  });

  it("makes exactly ONE call for ten concurrent refreshes of the same token", async () => {
    const { fetchImpl, calls, release } = deferredFetch(() =>
      jsonResponse(session("new-access", "new-refresh")),
    );

    const attempts = Array.from({ length: 10 }, () =>
      refreshSession("shared-refresh", { baseUrl: BASE, fetchImpl }),
    );

    // Registered synchronously, before any await: a second caller in the same
    // tick has to find the entry, or this is not a single-flight at all.
    expect(inFlightCount()).toBe(1);
    expect(calls).toHaveLength(1);

    release();

    const outcomes = await Promise.all(attempts);

    expect(calls).toHaveLength(1);
    expect(outcomes.every((outcome) => outcome.status === "refreshed")).toBe(true);
    // Every caller gets the same rotated token, so there is exactly one live
    // refresh token afterwards.
    const tokens = new Set(
      outcomes.map((outcome) =>
        outcome.status === "refreshed" ? outcome.tokens.refreshToken : "?",
      ),
    );

    expect(tokens).toEqual(new Set(["new-refresh"]));
  });

  it("does not make two different sessions wait on each other", async () => {
    const { fetchImpl, calls, release } = deferredFetch(() =>
      jsonResponse(session("a", "b")),
    );

    void refreshSession("token-for-user-a", { baseUrl: BASE, fetchImpl });
    void refreshSession("token-for-user-b", { baseUrl: BASE, fetchImpl });

    expect(calls).toHaveLength(2);
    expect(inFlightCount()).toBe(2);

    release();
    await Promise.resolve();
  });

  it("releases the in-flight slot once the request settles", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse(session("a", "b")));

    await refreshSession("t", { baseUrl: BASE, fetchImpl: fetchImpl as never });

    expect(inFlightCount()).toBe(0);
  });

  it("answers a straggler from the grace window instead of replaying the token", async () => {
    // The failure the end-to-end run caught: a request that was already in
    // flight when the rotation happened still carries the pre-rotation cookie.
    // Without the grace window it spends that token a second time, the API sees
    // a replay, and the session is revoked out from under everyone.
    const fetchImpl = vi.fn(async () => jsonResponse(session("new-access", "new-refresh")));
    const start = 1_000_000;

    const first = await refreshSession(
      "spent-token",
      { baseUrl: BASE, fetchImpl: fetchImpl as never },
      start,
    );

    expect(inFlightCount()).toBe(0);
    expect(graceCount()).toBe(1);

    const straggler = await refreshSession(
      "spent-token",
      { baseUrl: BASE, fetchImpl: fetchImpl as never },
      start + 1_000,
    );

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(straggler).toEqual(first);
  });

  it("stops answering from the grace window once it expires", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse(session("a", "b")));
    const start = 1_000_000;

    await refreshSession("t", { baseUrl: BASE, fetchImpl: fetchImpl as never }, start);
    await refreshSession(
      "t",
      { baseUrl: BASE, fetchImpl: fetchImpl as never },
      start + GRACE_MS + 1,
    );

    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it("remembers a rejection too, so a dead token is not retried in a storm", async () => {
    const fetchImpl = vi.fn(async () =>
      jsonResponse({ error: "session is no longer valid" }, 401),
    );
    const start = 1_000_000;

    await refreshSession("dead", { baseUrl: BASE, fetchImpl: fetchImpl as never }, start);
    const second = await refreshSession(
      "dead",
      { baseUrl: BASE, fetchImpl: fetchImpl as never },
      start + 500,
    );

    expect(fetchImpl).toHaveBeenCalledTimes(1);
    expect(second.status).toBe("rejected");
  });

  it("does NOT remember an unreachable API: the token was never spent", async () => {
    const fetchImpl = vi.fn(async () => {
      throw new TypeError("fetch failed");
    });
    const start = 1_000_000;

    await refreshSession("t", { baseUrl: BASE, fetchImpl: fetchImpl as never }, start);

    expect(graceCount()).toBe(0);

    await refreshSession(
      "t",
      { baseUrl: BASE, fetchImpl: fetchImpl as never },
      start + 100,
    );

    expect(fetchImpl).toHaveBeenCalledTimes(2);
  });

  it("prunes expired entries rather than growing forever", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse(session("a", "b")));
    const start = 1_000_000;

    for (let i = 0; i < 5; i += 1) {
      await refreshSession(
        `token-${i}`,
        { baseUrl: BASE, fetchImpl: fetchImpl as never },
        start,
      );
    }

    expect(graceCount()).toBe(5);

    await refreshSession(
      "later",
      { baseUrl: BASE, fetchImpl: fetchImpl as never },
      start + GRACE_MS + 1,
    );

    expect(graceCount()).toBe(1);
  });

  it("treats a 401 as the session being over", async () => {
    const outcome = await refreshSession("dead", {
      baseUrl: BASE,
      fetchImpl: (async () =>
        jsonResponse({ error: "session is no longer valid" }, 401)) as typeof fetch,
    });

    expect(outcome).toEqual({ status: "rejected", reason: "unauthorized" });
  });

  it("treats a 403 as the session being over", async () => {
    // ErrNoOrganization / ErrNotAMember. Retrying will never change the answer.
    const outcome = await refreshSession("orphaned", {
      baseUrl: BASE,
      fetchImpl: (async () =>
        jsonResponse({ error: "not a member of that organization" }, 403)) as typeof fetch,
    });

    expect(outcome.status).toBe("rejected");
  });

  it("does NOT treat an unreachable API as the session being over", async () => {
    // The difference that matters: `rejected` clears cookies, `unavailable`
    // leaves them. Getting this wrong signs every user out during an API blip.
    const outcome = await refreshSession("fine", {
      baseUrl: BASE,
      fetchImpl: (async () => {
        throw new TypeError("fetch failed");
      }) as typeof fetch,
    });

    expect(outcome.status).toBe("unavailable");
  });

  it("does not treat a 500 as the session being over either", async () => {
    const outcome = await refreshSession("fine", {
      baseUrl: BASE,
      fetchImpl: (async () =>
        jsonResponse({ error: "internal server error" }, 500)) as typeof fetch,
    });

    expect(outcome.status).toBe("unavailable");
  });

  it("shares a failure with every waiter rather than each retrying", async () => {
    const { fetchImpl, calls, release } = deferredFetch(() =>
      jsonResponse({ error: "session is no longer valid" }, 401),
    );

    const attempts = Array.from({ length: 5 }, () =>
      refreshSession("dead", { baseUrl: BASE, fetchImpl }),
    );

    release();

    const outcomes = await Promise.all(attempts);

    expect(calls).toHaveLength(1);
    expect(outcomes.every((outcome) => outcome.status === "rejected")).toBe(true);
  });
});
