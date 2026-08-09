/**
 * The 401 → refresh → retry-once rule, and the ways it must not misbehave:
 * no stampede, no second refresh, no loop.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

import { authenticatedCall } from "@/lib/api/authenticated";
import * as endpoints from "@/lib/api/endpoints";
import type { SessionTokens } from "@/lib/api/types";
import { __resetInFlightForTests } from "@/lib/session/refresh";

const BASE = "http://api.test/api/v1";

const PROJECTS_OK = {
  projects: [
    {
      id: "p1",
      name: "Launch",
      description: "",
      archived_at: null,
      created_at: "2026-08-08T10:00:00Z",
      updated_at: "2026-08-08T10:00:00Z",
    },
  ],
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const UNAUTHORIZED = () => json({ error: "authentication required" }, 401);

const SESSION_BODY = (access: string, refresh: string) => ({
  token_type: "Bearer",
  access_token: access,
  expires_in: 900,
  refresh_token: refresh,
  user_id: "u1",
  organization: { id: "o1", name: "Acme", slug: "acme", role: "owner" },
});

/**
 * A fake API that 401s every request bearing a stale token and succeeds for the
 * rotated one. Records every request so the tests can count them.
 */
function fakeApi(options: { refreshStatus?: number; refreshDelayMs?: number } = {}) {
  const requests: { url: string; token: string | undefined }[] = [];
  // What /auth/refresh hands out, and what the resource routes accept. They are
  // separate so a test can model a session that was revoked between the two.
  const issuedAccessToken = "fresh-access";
  let liveAccessToken = "fresh-access";

  const fetchImpl = (async (url: string, init: RequestInit) => {
    const headers = (init.headers ?? {}) as Record<string, string>;
    const token = headers.authorization?.replace("Bearer ", "");

    requests.push({ url, token });

    if (url.endsWith("/auth/refresh")) {
      if (options.refreshDelayMs !== undefined) {
        await new Promise((resolve) => setTimeout(resolve, options.refreshDelayMs));
      }

      if (options.refreshStatus !== undefined && options.refreshStatus !== 200) {
        return json({ error: "session is no longer valid" }, options.refreshStatus);
      }

      return json(SESSION_BODY(issuedAccessToken, "rotated-refresh"));
    }

    return token === liveAccessToken ? json(PROJECTS_OK) : UNAUTHORIZED();
  }) as unknown as typeof fetch;

  return {
    fetchImpl,
    requests,
    refreshCalls: () => requests.filter((r) => r.url.endsWith("/auth/refresh")).length,
    resourceCalls: () => requests.filter((r) => !r.url.endsWith("/auth/refresh")).length,
    setLiveToken: (value: string) => {
      liveAccessToken = value;
    },
  };
}

beforeEach(() => {
  __resetInFlightForTests();
});

describe("authenticatedCall", () => {
  it("returns the answer directly when the token is good", async () => {
    const api = fakeApi();

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: "fresh-access",
      refreshToken: "r",
      onRefreshed: vi.fn(),
      fetchImpl: api.fetchImpl,
    });

    expect(result.ok).toBe(true);
    expect(api.refreshCalls()).toBe(0);
  });

  it("refreshes on a 401 and retries the original request once", async () => {
    const api = fakeApi();
    const onRefreshed = vi.fn();

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: "expired-access",
      refreshToken: "r",
      onRefreshed,
      fetchImpl: api.fetchImpl,
    });

    expect(result.ok).toBe(true);
    expect(api.refreshCalls()).toBe(1);
    // The original request, then exactly one retry.
    expect(api.resourceCalls()).toBe(2);
    expect(api.requests.map((r) => r.token)).toEqual([
      "expired-access",
      undefined,
      "fresh-access",
    ]);
    // The rotated refresh token has to be persisted, or the cookie now holds a
    // token the API treats as a replay.
    expect(onRefreshed).toHaveBeenCalledTimes(1);
    expect((onRefreshed.mock.calls[0][0] as SessionTokens).refreshToken).toBe(
      "rotated-refresh",
    );
  });

  it("makes ONE refresh call for eight concurrent 401s", async () => {
    // The stampede case. Without the single-flight this is eight refreshes:
    // one rotation and seven replays, and the API revokes the session.
    const api = fakeApi({ refreshDelayMs: 5 });
    const onRefreshed = vi.fn();

    const results = await Promise.all(
      Array.from({ length: 8 }, () =>
        authenticatedCall(endpoints.listProjects(), {
          baseUrl: BASE,
          accessToken: "expired-access",
          refreshToken: "the-one-refresh-token",
          onRefreshed,
          fetchImpl: api.fetchImpl,
        }),
      ),
    );

    expect(api.refreshCalls()).toBe(1);
    expect(results.every((result) => result.ok)).toBe(true);
    // Eight originals + eight retries.
    expect(api.resourceCalls()).toBe(16);
  });

  it("does not refresh for a 403, 404, 409 or 429", async () => {
    for (const status of [403, 404, 409, 429]) {
      __resetInFlightForTests();

      const calls: string[] = [];
      const fetchImpl = (async (url: string) => {
        calls.push(url);

        return json({ error: "no" }, status);
      }) as unknown as typeof fetch;

      const result = await authenticatedCall(endpoints.listProjects(), {
        baseUrl: BASE,
        accessToken: "fresh",
        refreshToken: "r",
        onRefreshed: vi.fn(),
        fetchImpl,
      });

      expect(result.ok).toBe(false);
      expect(calls.filter((url) => url.endsWith("/auth/refresh"))).toHaveLength(0);
    }
  });

  it("signs the caller out when the refresh is rejected, and stops there", async () => {
    const api = fakeApi({ refreshStatus: 401 });
    const onSignedOut = vi.fn();

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: "expired-access",
      refreshToken: "dead-refresh",
      onRefreshed: vi.fn(),
      onSignedOut,
      fetchImpl: api.fetchImpl,
    });

    expect(result.ok).toBe(false);
    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(onSignedOut).toHaveBeenCalledTimes(1);
    // One refresh attempt, and no retry of the original: nothing loops.
    expect(api.refreshCalls()).toBe(1);
    expect(api.resourceCalls()).toBe(1);
  });

  it("does not sign the caller out when the refresh endpoint is unreachable", async () => {
    const onSignedOut = vi.fn();
    const fetchImpl = (async (url: string) => {
      if (url.endsWith("/auth/refresh")) {
        throw new TypeError("fetch failed");
      }

      return UNAUTHORIZED();
    }) as unknown as typeof fetch;

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: "expired",
      refreshToken: "perfectly-good",
      onRefreshed: vi.fn(),
      onSignedOut,
      fetchImpl,
    });

    expect(result.ok).toBe(false);
    expect(onSignedOut).not.toHaveBeenCalled();
  });

  it("retries at most once, even when the retry also 401s", async () => {
    // A revoked session that still has a rotatable refresh token would loop
    // here if the retry were itself eligible for a refresh.
    const api = fakeApi();

    api.setLiveToken("some-other-token-nobody-has");

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: "expired",
      refreshToken: "r",
      onRefreshed: vi.fn(),
      fetchImpl: api.fetchImpl,
    });

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(api.refreshCalls()).toBe(1);
    expect(api.resourceCalls()).toBe(2);
  });

  it("refreshes up-front when there is no access token at all", async () => {
    const api = fakeApi();

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: null,
      refreshToken: "r",
      onRefreshed: vi.fn(),
      fetchImpl: api.fetchImpl,
    });

    expect(result.ok).toBe(true);
    // No pointless request that we already knew would 401.
    expect(api.resourceCalls()).toBe(1);
  });

  it("spends the refresh token ONCE even when the post-refresh request 401s", async () => {
    // The up-front refresh and the 401-recovery used to be independent, so this
    // path refreshed twice — and the second attempt presented the pre-rotation
    // token, which the API treats as a replay and revokes the session for. The
    // grace window masks it most of the time, which is what made it worth a flag
    // rather than a comment.
    const api = fakeApi();

    // The session is revoked between the refresh and the retry, so the freshly
    // issued access token does not work either.
    api.setLiveToken("a-token-nobody-will-ever-hold");

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: null,
      refreshToken: "r",
      onRefreshed: vi.fn(),
      fetchImpl: api.fetchImpl,
    });

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(api.refreshCalls()).toBe(1);
    expect(api.resourceCalls()).toBe(1);
  });

  it("answers 401 without a round trip when there is no session", async () => {
    const fetchImpl = vi.fn();

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: null,
      refreshToken: null,
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("will not refresh for a caller that cannot persist the result", async () => {
    // This is the Server Component case. Refreshing here would rotate the token
    // and drop the successor on the floor, costing the user their session for
    // the sake of one render.
    const api = fakeApi();

    const result = await authenticatedCall(endpoints.listProjects(), {
      baseUrl: BASE,
      accessToken: "expired",
      refreshToken: "r",
      // no onRefreshed
      fetchImpl: api.fetchImpl,
    });

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(api.refreshCalls()).toBe(0);
  });
});
