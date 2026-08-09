/**
 * `proxy.ts`'s decision, and the two properties that keep a failed refresh from
 * becoming a loop:
 *
 *   - a rejected refresh clears the cookies, so the *next* request has nothing
 *     to refresh with and makes no API call at all;
 *   - nothing in `proxy.ts` redirects, so there is no navigation to loop.
 */

import { NextRequest, NextResponse } from "next/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

import {
  ACCESS_COOKIE,
  REFRESH_COOKIE,
  SESSION_COOKIE,
  encodeMetadata,
  metadataFromTokens,
} from "@/lib/session/cookies";
import {
  FORWARDED_SESSION_HEADER,
  decodeForwardedSession,
  encodeForwardedSession,
  stripForwardedSession,
} from "@/lib/session/forward";
import { resolveProxySession } from "@/lib/session/proxy-session";
import { __resetInFlightForTests } from "@/lib/session/refresh";
import type { SessionTokens } from "@/lib/api/types";
import proxy, { config as proxyConfig } from "@/proxy";

const BASE = "http://api.test/api/v1";

const TOKENS: SessionTokens = {
  tokenType: "Bearer",
  accessToken: "fresh.access.token",
  expiresIn: 900,
  refreshToken: "rotated-refresh",
  userId: "u1",
  organization: { id: "o1", name: "Acme", slug: "acme", role: "owner" },
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const REFRESH_OK = () =>
  json({
    token_type: "Bearer",
    access_token: TOKENS.accessToken,
    expires_in: 900,
    refresh_token: TOKENS.refreshToken,
    user_id: "u1",
    organization: TOKENS.organization,
  });

beforeEach(() => {
  __resetInFlightForTests();
});

describe("resolveProxySession", () => {
  const now = 1_700_000_000_000;
  const freshMetadata = metadataFromTokens({ ...TOKENS, expiresIn: 900 }, now);

  it("leaves a fresh session alone without calling the API", async () => {
    const fetchImpl = vi.fn();

    const action = await resolveProxySession(
      { accessToken: "at", refreshToken: "rt", metadata: freshMetadata },
      now,
      { baseUrl: BASE, fetchImpl: fetchImpl as unknown as typeof fetch },
    );

    expect(action).toEqual({ kind: "unchanged" });
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("refreshes an expired access token rather than letting the render 401", async () => {
    const action = await resolveProxySession(
      { accessToken: "stale", refreshToken: "rt", metadata: freshMetadata },
      // Well past expiry: the "came back after lunch" case.
      now + 20 * 60 * 1000,
      { baseUrl: BASE, fetchImpl: REFRESH_OK as unknown as typeof fetch },
    );

    expect(action.kind).toBe("refreshed");
  });

  it("refreshes when the access cookie is gone but the refresh cookie is not", async () => {
    const action = await resolveProxySession(
      { accessToken: null, refreshToken: "rt", metadata: null },
      now,
      { baseUrl: BASE, fetchImpl: REFRESH_OK as unknown as typeof fetch },
    );

    expect(action.kind).toBe("refreshed");
  });

  it("does nothing at all when there is no session", async () => {
    const fetchImpl = vi.fn();

    const action = await resolveProxySession(
      { accessToken: null, refreshToken: null, metadata: null },
      now,
      { baseUrl: BASE, fetchImpl: fetchImpl as unknown as typeof fetch },
    );

    expect(action).toEqual({ kind: "unchanged" });
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("clears a half-state: an access cookie with no refresh cookie", async () => {
    const action = await resolveProxySession(
      { accessToken: "orphan", refreshToken: null, metadata: freshMetadata },
      now,
      { baseUrl: BASE },
    );

    expect(action).toEqual({ kind: "signed-out", reason: "no_refresh_token" });
  });

  it("signs out on a rejected refresh", async () => {
    const action = await resolveProxySession(
      { accessToken: null, refreshToken: "dead", metadata: null },
      now,
      {
        baseUrl: BASE,
        fetchImpl: (async () => json({ error: "session is no longer valid" }, 401)) as typeof fetch,
      },
    );

    expect(action.kind).toBe("signed-out");
  });

  it("leaves the session alone when the API is unreachable", async () => {
    const action = await resolveProxySession(
      { accessToken: null, refreshToken: "good", metadata: null },
      now,
      {
        baseUrl: BASE,
        fetchImpl: (async () => {
          throw new TypeError("fetch failed");
        }) as typeof fetch,
      },
    );

    expect(action).toEqual({ kind: "unchanged" });
  });
});

describe("forwarded session header", () => {
  it("round-trips, and carries no refresh token", () => {
    const encoded = encodeForwardedSession({
      accessToken: TOKENS.accessToken,
      metadata: metadataFromTokens(TOKENS, 1000),
    });

    expect(Buffer.from(encoded, "base64url").toString("utf8")).not.toContain(
      TOKENS.refreshToken,
    );
    expect(decodeForwardedSession(encoded)?.accessToken).toBe(TOKENS.accessToken);
  });

  it("rejects anything it did not write", () => {
    expect(decodeForwardedSession(null)).toBeNull();
    expect(decodeForwardedSession("garbage")).toBeNull();
    expect(
      decodeForwardedSession(Buffer.from('{"accessToken":"x"}').toString("base64url")),
    ).toBeNull();
  });

  it("strips an inbound value", () => {
    const headers = new Headers({ [FORWARDED_SESSION_HEADER]: "attacker-supplied" });

    expect(stripForwardedSession(headers).get(FORWARDED_SESSION_HEADER)).toBeNull();
    // The original is not mutated — the caller decides what to do with it.
    expect(headers.get(FORWARDED_SESSION_HEADER)).toBe("attacker-supplied");
  });
});

/** Builds a request carrying whatever session cookies a test wants. */
function requestWith(cookies: Record<string, string>, headers: HeadersInit = {}) {
  const request = new NextRequest("http://localhost:3000/boards/b1", { headers });

  for (const [name, value] of Object.entries(cookies)) {
    request.cookies.set(name, value);
  }

  return request;
}

describe("proxy", () => {
  it("never redirects, on any path", async () => {
    // The structural answer to "a failed refresh must not become a redirect
    // loop": this file has no redirect in it, so the loop is unreachable rather
    // than guarded against.
    const { readFile } = await import("node:fs/promises");
    const { join } = await import("node:path");
    // Comments stripped: the file explains at length why it does not redirect,
    // and that explanation must not be what the assertion is reading.
    const code = (await readFile(join(process.cwd(), "proxy.ts"), "utf8"))
      .replace(/\/\*[\s\S]*?\*\//g, "")
      .replace(/^\s*\/\/.*$/gm, "");

    expect(code).not.toMatch(/NextResponse\.redirect/);
    expect(code).not.toMatch(/NextResponse\.rewrite/);
    expect(code).toMatch(/NextResponse\.next/);
  });

  it("runs on this app's own API routes, so it can strip the session header", () => {
    // It must run there — a path this does not run on is a path where a client
    // can supply its own x-collabboard-session and be believed. What it must not
    // do there is refresh, which the next test covers.
    const matcher = proxyConfig.matcher[0];
    const pattern = new RegExp(`^${matcher}$`);

    expect(pattern.test("/api/auth/refresh")).toBe(true);
    expect(pattern.test("/api/proxy/projects")).toBe(true);
    expect(pattern.test("/boards/b1")).toBe(true);
    expect(pattern.test("/")).toBe(true);
    // Build output carries no session and must not wait on anything.
    expect(pattern.test("/_next/static/chunk.js")).toBe(false);
    expect(pattern.test("/_next/image")).toBe(false);
  });

  it("does not refresh on /api/* or /healthz, only strips", async () => {
    // One refresher per request. The Route Handlers refresh for themselves; if
    // this refreshed there too, both would spend the same refresh token and the
    // second spend is a replay the API revokes the session for. /healthz is
    // excluded so a readiness probe cannot put an API round trip in front of
    // itself.
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    for (const path of ["/api/auth/refresh", "/api/proxy/me", "/healthz"]) {
      const request = new NextRequest(`http://localhost:3000${path}`);

      request.cookies.set(REFRESH_COOKIE, "expired-but-valid");

      const response = await proxy(request);

      expect(response.headers.getSetCookie()).toHaveLength(0);
    }

    expect(fetchImpl).not.toHaveBeenCalled();

    vi.unstubAllGlobals();
  });

  it("strips a forged session header on an API route", async () => {
    const forgedHeader = encodeForwardedSession({
      accessToken: "forged.token",
      metadata: metadataFromTokens(TOKENS),
    });

    const request = new NextRequest("http://localhost:3000/api/proxy/me", {
      headers: { [FORWARDED_SESSION_HEADER]: forgedHeader },
    });

    const response = await proxy(request);

    expect(response.headers.get(FORWARDED_SESSION_HEADER)).toBeNull();
  });

  it("passes a fresh session through untouched", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const request = requestWith({
      [ACCESS_COOKIE]: "at",
      [REFRESH_COOKIE]: "rt",
      [SESSION_COOKIE]: encodeMetadata(metadataFromTokens(TOKENS)),
    });

    const response = await proxy(request);

    expect(response.headers.getSetCookie()).toHaveLength(0);
    expect(fetchImpl).not.toHaveBeenCalled();

    vi.unstubAllGlobals();
  });

  it("strips a client-supplied session header even when it does nothing else", async () => {
    const forged = encodeForwardedSession({
      accessToken: "forged.token",
      metadata: metadataFromTokens(TOKENS),
    });

    const request = requestWith({}, { [FORWARDED_SESSION_HEADER]: forged });
    const response = await proxy(request);

    // NextResponse.next() surfaces the overridden request headers on the
    // response object it builds; what matters is that ours is not the forged
    // one. With no session there is nothing to forward, so there must be no
    // value at all.
    expect(response.headers.get(FORWARDED_SESSION_HEADER)).toBeNull();
  });

  it("refreshes, sets new cookies, and forwards the new access token", async () => {
    vi.stubGlobal("fetch", REFRESH_OK as unknown as typeof fetch);

    const request = requestWith({ [REFRESH_COOKIE]: "expired-but-valid" });
    const response = await proxy(request);
    const setCookie = response.headers.getSetCookie();

    expect(setCookie).toHaveLength(3);
    expect(setCookie.every((header) => /HttpOnly/i.test(header))).toBe(true);
    expect(setCookie.some((header) => header.startsWith(`${REFRESH_COOKIE}=`))).toBe(
      true,
    );

    vi.unstubAllGlobals();
  });

  it("clears the cookies on a rejected refresh, and the next request refreshes nothing", async () => {
    const calls: string[] = [];
    const fetchImpl = (async (url: string) => {
      calls.push(url);

      return json({ error: "session is no longer valid" }, 401);
    }) as unknown as typeof fetch;

    vi.stubGlobal("fetch", fetchImpl);

    const first = await proxy(requestWith({ [REFRESH_COOKIE]: "dead" }));

    expect(calls).toHaveLength(1);
    expect(first.headers.getSetCookie().every((h) => /Max-Age=0/i.test(h))).toBe(true);
    // Nothing that could send the browser anywhere. Signed out is a state.
    expect(first.status).toBe(200);
    expect(first.headers.get("location")).toBeNull();

    // The browser now sends no cookies, because the ones it had were expired.
    // The second request costs nothing — which is what "not a loop" means in a
    // layer that does not navigate.
    __resetInFlightForTests();

    const second = await proxy(requestWith({}));

    expect(calls).toHaveLength(1);
    expect(second.headers.getSetCookie()).toHaveLength(0);
    expect(second.headers.get("location")).toBeNull();

    vi.unstubAllGlobals();
  });

  it("leaves the cookies in place when the API is down", async () => {
    vi.stubGlobal("fetch", (async () => {
      throw new TypeError("fetch failed");
    }) as typeof fetch);

    const response = await proxy(requestWith({ [REFRESH_COOKIE]: "perfectly-good" }));

    // An API blip must not sign every user out.
    expect(response.headers.getSetCookie()).toHaveLength(0);

    vi.unstubAllGlobals();
  });
});

describe("NextResponse plumbing", () => {
  it("writes the session cookies the proxy relies on", () => {
    // Guards the assumption that ResponseCookies satisfies our CookieWriter.
    const response = NextResponse.next();

    expect(() =>
      response.cookies.set("x", "y", {
        httpOnly: true,
        secure: true,
        sameSite: "lax",
        path: "/",
        maxAge: 1,
      }),
    ).not.toThrow();
  });
});
