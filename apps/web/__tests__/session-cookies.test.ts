/**
 * The claim this whole design rests on: **client JavaScript cannot read the
 * refresh token.**
 *
 * It is asserted three ways here, none of which is "we passed `httpOnly: true`
 * and assume that worked":
 *
 * 1. The real `Set-Cookie` headers Next serializes carry `HttpOnly`.
 * 2. A real cookie store, given those exact headers over HTTP, refuses to
 *    expose them to `document.cookie`. That is the browser rule itself, run.
 * 3. No token appears in any response body this app returns to a browser
 *    (`__tests__/auth-routes.test.ts`).
 */

import { NextResponse } from "next/server";
import { JSDOM } from "jsdom";
import { describe, expect, it } from "vitest";

import {
  ACCESS_COOKIE,
  EXPIRY_SKEW_SECONDS,
  REFRESH_COOKIE,
  SESSION_COOKIE,
  SESSION_COOKIE_NAMES,
  accessTokenIsStale,
  clearSessionCookies,
  cookiesAreSecure,
  decodeMetadata,
  encodeMetadata,
  metadataFromTokens,
  readSessionCookies,
  sessionCookieOptions,
  writeSessionCookies,
} from "@/lib/session/cookies";
import type { SessionTokens } from "@/lib/api/types";

const TOKENS: SessionTokens = {
  tokenType: "Bearer",
  accessToken: "header.payload.signature",
  expiresIn: 900,
  refreshToken: "s3cr3t-refresh-token-value",
  userId: "11111111-1111-4111-8111-111111111111",
  organization: { id: "org-1", name: "Acme", slug: "acme", role: "owner" },
};

/** The real Set-Cookie headers, produced by Next's own serializer. */
function setCookieHeaders(env: Record<string, string | undefined>): string[] {
  const response = NextResponse.next();

  writeSessionCookies(response.cookies, TOKENS, Date.now(), env);

  return response.headers.getSetCookie();
}

describe("session cookie attributes", () => {
  it("marks every cookie HttpOnly, SameSite=Lax and Secure in production", () => {
    const headers = setCookieHeaders({ NODE_ENV: "production" });

    expect(headers).toHaveLength(3);

    for (const header of headers) {
      expect(header).toMatch(/HttpOnly/i);
      expect(header).toMatch(/SameSite=lax/i);
      expect(header).toMatch(/Secure/i);
      expect(header).toMatch(/Path=\//i);
    }
  });

  it("drops Secure outside production, where the dev server is plain HTTP", () => {
    // Not a weakening: a Secure cookie over http://localhost is silently
    // discarded by the browser, and the failure looks like "login does nothing".
    for (const header of setCookieHeaders({ NODE_ENV: "development" })) {
      expect(header).toMatch(/HttpOnly/i);
      expect(header).not.toMatch(/Secure/i);
    }

    expect(cookiesAreSecure({ NODE_ENV: "production" })).toBe(true);
    expect(cookiesAreSecure({ NODE_ENV: "test" })).toBe(false);
  });

  it("names all three cookies in SESSION_COOKIE_NAMES, so clearing is complete", () => {
    const written = setCookieHeaders({ NODE_ENV: "production" }).map(
      (header) => header.split("=")[0],
    );

    expect(new Set(written)).toEqual(new Set(SESSION_COOKIE_NAMES));
  });

  it("passes httpOnly on every single set() call, not just in aggregate", () => {
    const calls: { name: string; options: { httpOnly: boolean } }[] = [];

    writeSessionCookies(
      { set: (name, _value, options) => calls.push({ name, options }) },
      TOKENS,
    );

    expect(calls.map((call) => call.name)).toEqual([
      ACCESS_COOKIE,
      REFRESH_COOKIE,
      SESSION_COOKIE,
    ]);
    expect(calls.every((call) => call.options.httpOnly)).toBe(true);
  });
});

describe("a browser cannot read the refresh token", () => {
  it("does not expose any session cookie to document.cookie", async () => {
    // jsdom's cookie store is tough-cookie — the same implementation the HttpOnly
    // rule is written against. Cookies are inserted the way a server sets them
    // (over HTTP), then read the way a script would.
    const dom = new JSDOM("<!doctype html><html><body></body></html>", {
      url: "https://collabboard.example/",
    });

    for (const header of setCookieHeaders({ NODE_ENV: "production" })) {
      dom.cookieJar.setCookieSync(header, "https://collabboard.example/", {
        http: true,
      });
    }

    // The cookies really are stored — the jar has them, so the browser would
    // send them back on the next request.
    const stored = dom.cookieJar.getCookieStringSync("https://collabboard.example/", {
      http: true,
    });

    expect(stored).toContain(REFRESH_COOKIE);

    // And script sees none of them.
    const visibleToScript = dom.window.document.cookie;

    expect(visibleToScript).toBe("");
    expect(visibleToScript).not.toContain(TOKENS.refreshToken);
    expect(visibleToScript).not.toContain(TOKENS.accessToken);

    dom.window.close();
  });

  it("refuses a script's attempt to overwrite the refresh cookie", async () => {
    const dom = new JSDOM("<!doctype html><html><body></body></html>", {
      url: "https://collabboard.example/",
    });

    for (const header of setCookieHeaders({ NODE_ENV: "production" })) {
      dom.cookieJar.setCookieSync(header, "https://collabboard.example/", {
        http: true,
      });
    }

    dom.window.document.cookie = `${REFRESH_COOKIE}=attacker-controlled; Path=/`;

    const stored = dom.cookieJar.getCookieStringSync("https://collabboard.example/", {
      http: true,
    });

    expect(stored).toContain(TOKENS.refreshToken);
    expect(stored).not.toContain("attacker-controlled");

    dom.window.close();
  });
});

describe("session metadata", () => {
  it("round-trips through the cookie encoding", () => {
    const metadata = metadataFromTokens(TOKENS, 1_000_000);

    expect(decodeMetadata(encodeMetadata(metadata))).toEqual(metadata);
    expect(metadata.accessExpiresAt).toBe(1_000_000 + 900_000);
  });

  it("uses a cookie-safe encoding", () => {
    const encoded = encodeMetadata(metadataFromTokens(TOKENS));

    expect(encoded).toMatch(/^[A-Za-z0-9_-]+$/);
  });

  it("returns null for anything it did not write", () => {
    expect(decodeMetadata(undefined)).toBeNull();
    expect(decodeMetadata("")).toBeNull();
    expect(decodeMetadata("not-base64-json")).toBeNull();
    expect(decodeMetadata(Buffer.from("{}").toString("base64url"))).toBeNull();
    expect(
      decodeMetadata(
        Buffer.from(JSON.stringify({ userId: "u", accessExpiresAt: 1 })).toString(
          "base64url",
        ),
      ),
    ).toBeNull();
  });
});

describe("readSessionCookies", () => {
  it("reads all three, and reports missing ones as null", () => {
    const jar = new Map([
      [ACCESS_COOKIE, "at"],
      [REFRESH_COOKIE, "rt"],
      [SESSION_COOKIE, encodeMetadata(metadataFromTokens(TOKENS, 500))],
    ]);

    const stored = readSessionCookies({
      get: (name) => (jar.has(name) ? { value: jar.get(name)! } : undefined),
    });

    expect(stored.accessToken).toBe("at");
    expect(stored.refreshToken).toBe("rt");
    expect(stored.metadata?.organization.slug).toBe("acme");

    const empty = readSessionCookies({ get: () => undefined });

    expect(empty).toEqual({ accessToken: null, refreshToken: null, metadata: null });
  });
});

describe("clearSessionCookies", () => {
  it("expires every cookie with the same attributes it set them with", () => {
    const response = NextResponse.next();

    clearSessionCookies(response.cookies, { NODE_ENV: "production" });

    const headers = response.headers.getSetCookie();

    expect(headers).toHaveLength(3);

    for (const header of headers) {
      expect(header).toMatch(/Max-Age=0/i);
      expect(header).toMatch(/Path=\//i);
      expect(header).toMatch(/HttpOnly/i);
    }
  });
});

describe("accessTokenIsStale", () => {
  const options = sessionCookieOptions({ NODE_ENV: "production" });

  it("uses the shared max-age for all three cookies", () => {
    // If the access cookie expired at 15 minutes we would have two sources of
    // truth about freshness. accessExpiresAt is the only one.
    expect(options.maxAge).toBeGreaterThan(13 * 24 * 60 * 60);
  });

  it("treats a token inside the skew window as already stale", () => {
    const now = 1_000_000_000;
    const metadata = { ...metadataFromTokens(TOKENS, now) };

    expect(accessTokenIsStale(metadata, now)).toBe(false);
    expect(
      accessTokenIsStale(metadata, metadata.accessExpiresAt - EXPIRY_SKEW_SECONDS * 1000),
    ).toBe(true);
    expect(accessTokenIsStale(metadata, metadata.accessExpiresAt + 1)).toBe(true);
  });

  it("treats missing metadata as stale", () => {
    expect(accessTokenIsStale(null)).toBe(true);
  });
});
