/**
 * The browser half of the refresh rule.
 *
 * Same two properties as the server half — one refresh for N concurrent 401s,
 * one retry — but the browser reaches `/api/auth/refresh` on this origin rather
 * than the API, because it has no token to present.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

import {
  __resetBrowserApiForTests,
  BROWSER_API_BASE,
  browserApi,
  onSignedOut,
  REFRESH_ENDPOINT,
  refreshInFlight,
  refreshOnce,
  signOut,
} from "@/lib/api/browser";
import * as endpoints from "@/lib/api/endpoints";

const CARDS_OK = {
  cards: [
    {
      id: "c1",
      board_id: "b1",
      column_id: "col1",
      title: "Ship it",
      description: "",
      assignee_id: null,
      due_at: null,
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

/**
 * A fake same-origin server: `/api/proxy/*` 401s until `/api/auth/refresh` has
 * been called, exactly like the real pair behaves once the cookie is stale.
 */
function fakeOrigin(options: { refreshStatus?: number; delayMs?: number } = {}) {
  const calls: string[] = [];
  let refreshed = false;

  const fetchImpl = (async (url: string) => {
    calls.push(url);

    if (url === REFRESH_ENDPOINT) {
      if (options.delayMs !== undefined) {
        await new Promise((resolve) => setTimeout(resolve, options.delayMs));
      }

      const status = options.refreshStatus ?? 204;

      if (status === 204) {
        refreshed = true;

        return new Response(null, { status: 204 });
      }

      return json({ error: "Your session has expired. Sign in again." }, status);
    }

    return refreshed ? json(CARDS_OK) : json({ error: "authentication required" }, 401);
  }) as unknown as typeof fetch;

  return {
    fetchImpl,
    calls,
    refreshCalls: () => calls.filter((url) => url === REFRESH_ENDPOINT).length,
    proxyCalls: () => calls.filter((url) => url.startsWith(BROWSER_API_BASE)).length,
  };
}

beforeEach(() => {
  __resetBrowserApiForTests();
});

describe("browserApi", () => {
  it("sends requests to this origin, never to the API", async () => {
    const origin = fakeOrigin();
    const api = browserApi({ fetchImpl: origin.fetchImpl });

    await api(endpoints.listCardsByBoard("b1"));

    // No absolute URL anywhere: nothing about the API's location is in the
    // client bundle, which is why #16's runtime contract holds for free here.
    for (const url of origin.calls) {
      expect(url.startsWith("/")).toBe(true);
    }

    expect(origin.calls).toContain(`${BROWSER_API_BASE}/boards/b1/cards`);
  });

  it("attaches same-origin credentials, which is what authenticates the call", async () => {
    const fetchImpl = vi.fn(async () => json(CARDS_OK));

    await browserApi({ fetchImpl: fetchImpl as unknown as typeof fetch })(
      endpoints.listCardsByBoard("b1"),
    );

    const [, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];

    expect(init.credentials).toBe("same-origin");
  });

  it("refreshes on a 401 and retries once", async () => {
    const origin = fakeOrigin();
    const api = browserApi({ fetchImpl: origin.fetchImpl });

    const result = await api(endpoints.listCardsByBoard("b1"));

    expect(result.ok).toBe(true);
    expect(origin.refreshCalls()).toBe(1);
    expect(origin.proxyCalls()).toBe(2);
  });

  it("makes ONE refresh call for six concurrent 401s", async () => {
    const origin = fakeOrigin({ delayMs: 5 });
    const api = browserApi({ fetchImpl: origin.fetchImpl });

    const results = await Promise.all(
      ["b1", "b2", "b3", "b4", "b5", "b6"].map((board) =>
        api(endpoints.listCardsByBoard(board)),
      ),
    );

    expect(origin.refreshCalls()).toBe(1);
    expect(results.every((result) => result.ok)).toBe(true);
    expect(refreshInFlight()).toBe(false);
  });

  it("does not retry a 403, 404, 409 or 429", async () => {
    for (const status of [403, 404, 409, 429]) {
      __resetBrowserApiForTests();

      const calls: string[] = [];
      const fetchImpl = (async (url: string) => {
        calls.push(url);

        return json({ error: "no" }, status);
      }) as unknown as typeof fetch;

      const result = await browserApi({ fetchImpl })(endpoints.listProjects());

      expect(result.ok).toBe(false);
      expect(calls).toHaveLength(1);
    }
  });

  it("returns the 401 and does not loop when the refresh is rejected", async () => {
    const origin = fakeOrigin({ refreshStatus: 401 });
    const api = browserApi({ fetchImpl: origin.fetchImpl });
    const listener = vi.fn();

    onSignedOut(listener);

    const result = await api(endpoints.listCardsByBoard("b1"));

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(origin.refreshCalls()).toBe(1);
    // The original request only. No retry, no second refresh.
    expect(origin.proxyCalls()).toBe(1);
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it("announces the sign-out once, not once per waiting request", async () => {
    const origin = fakeOrigin({ refreshStatus: 401, delayMs: 5 });
    const api = browserApi({ fetchImpl: origin.fetchImpl });
    const listener = vi.fn();

    onSignedOut(listener);

    await Promise.all([
      api(endpoints.listProjects()),
      api(endpoints.listMembers()),
      api(endpoints.currentUser()),
    ]);

    expect(origin.refreshCalls()).toBe(1);
    expect(listener).toHaveBeenCalledTimes(1);
  });

  it("does not announce a sign-out when the browser is simply offline", async () => {
    const listener = vi.fn();

    onSignedOut(listener);

    const result = await refreshOnce({
      fetchImpl: (async () => {
        throw new TypeError("Failed to fetch");
      }) as typeof fetch,
    });

    expect(result).toBe(false);
    expect(listener).not.toHaveBeenCalled();
  });

  it("does not announce a sign-out for a 502 from the refresh route", async () => {
    const listener = vi.fn();

    onSignedOut(listener);

    await refreshOnce({
      fetchImpl: (async () => json({ error: "upstream" }, 502)) as typeof fetch,
    });

    expect(listener).not.toHaveBeenCalled();
  });

  it("unsubscribes a listener", async () => {
    const listener = vi.fn();
    const unsubscribe = onSignedOut(listener);

    unsubscribe();

    await refreshOnce({
      fetchImpl: (async () => json({ error: "gone" }, 401)) as typeof fetch,
    });

    expect(listener).not.toHaveBeenCalled();
  });
});

describe("signOut", () => {
  it("announces the sign-out even when the logout call fails", async () => {
    // The cookies are cleared by the Route Handler regardless, so the browser is
    // signed out whatever the network did. Reporting otherwise strands the user.
    const listener = vi.fn();

    onSignedOut(listener);

    await signOut({
      fetchImpl: (async () => {
        throw new TypeError("Failed to fetch");
      }) as typeof fetch,
    });

    expect(listener).toHaveBeenCalledTimes(1);
  });
});

describe("no navigation in this layer", () => {
  it("exposes a subscription rather than performing a redirect", async () => {
    // A redirect issued from inside a fetch helper runs on every path, including
    // whatever the sign-in page turns out to be — which is exactly how a
    // sign-in page ends up redirecting itself. The screen decides; this only
    // reports. Asserted here so the property survives someone "helpfully"
    // adding a router.push().
    const { readFile } = await import("node:fs/promises");
    const { join } = await import("node:path");
    const source = await readFile(join(process.cwd(), "lib/api/browser.ts"), "utf8");

    expect(source).not.toMatch(/location\s*[.=]/);
    expect(source).not.toMatch(/router\.(push|replace)/);
    expect(source).not.toMatch(/redirect\(/);
  });
});
