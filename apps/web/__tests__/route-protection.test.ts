/**
 * Route protection: the redirect, where it gets the destination from, and why
 * that destination cannot be an open redirect.
 *
 * `proxy.ts` stamps the requested path onto the request because a layout is not
 * told its own URL. That header is client-supplied on the way in, so the two
 * halves are tested separately: the proxy overwrites it on every path, and the
 * reader launders it through `safeReturnPath` regardless.
 */

import { NextRequest } from "next/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

let requestHeaders = new Headers();
let cookieJar = new Map<string, string>();
const redirected: string[] = [];

vi.mock("next/headers", () => ({
  headers: async () => requestHeaders,
  cookies: async () => ({
    get: (name: string) => {
      const value = cookieJar.get(name);

      return value === undefined ? undefined : { name, value };
    },
  }),
}));

vi.mock("next/navigation", () => ({
  redirect: (target: string) => {
    redirected.push(target);
    // The real `redirect` throws a control-flow signal Next catches, which is
    // what makes `requireSession`'s "returns a session or does not return" type
    // honest. Throwing here keeps the tests on the same control flow.
    throw new Error(`NEXT_REDIRECT:${target}`);
  },
}));

const { REQUEST_PATH_HEADER, currentRequestPath, setRequestPath } = await import(
  "@/lib/session/request-path"
);
const { requireSession } = await import("@/lib/session/require");
const { SESSION_COOKIE, ACCESS_COOKIE, encodeMetadata } = await import(
  "@/lib/session/cookies"
);
const proxy = (await import("@/proxy")).default;

function signedIn(): void {
  cookieJar.set(ACCESS_COOKIE, "header.payload.signature");
  cookieJar.set(
    SESSION_COOKIE,
    encodeMetadata({
      userId: "u1",
      organization: { id: "o1", name: "Acme", slug: "acme", role: "owner" },
      accessExpiresAt: Date.now() + 600_000,
    }),
  );
}

beforeEach(() => {
  requestHeaders = new Headers();
  cookieJar = new Map();
  redirected.length = 0;
});

describe("proxy.ts stamps the requested path", () => {
  /** The header a Server Component will see, as `NextResponse.next` encodes it. */
  const forwarded = (response: Response, name: string) =>
    response.headers.get(`x-middleware-request-${name}`);

  it("puts the path and query on the request", async () => {
    const response = await proxy(new NextRequest("http://localhost:3000/app?tab=inbox"));

    expect(forwarded(response, REQUEST_PATH_HEADER)).toBe("/app?tab=inbox");
  });

  it("overwrites a value the client supplied, which is how it is stripped", async () => {
    const response = await proxy(
      new NextRequest("http://localhost:3000/app", {
        headers: { [REQUEST_PATH_HEADER]: "https://evil.example" },
      }),
    );

    expect(forwarded(response, REQUEST_PATH_HEADER)).toBe("/app");
  });

  it("stamps it on /api/* too, where nothing else happens", async () => {
    const response = await proxy(
      new NextRequest("http://localhost:3000/api/auth/session", {
        headers: { [REQUEST_PATH_HEADER]: "/forged" },
      }),
    );

    expect(forwarded(response, REQUEST_PATH_HEADER)).toBe("/api/auth/session");
  });

  it("still does not redirect anything", async () => {
    // ADR 0007: the loop that catches the sign-in page is not reachable rather
    // than merely guarded against, because this file contains no redirect.
    const response = await proxy(new NextRequest("http://localhost:3000/app"));

    expect(response.status).toBe(200);
    expect(response.headers.get("location")).toBeNull();
  });
});

describe("setRequestPath", () => {
  it("writes the path and query, and only those", () => {
    const target = new Headers();

    setRequestPath(target, new URL("http://localhost:3000/app?tab=inbox#card-3"));

    // No origin and no fragment: the fragment never leaves the browser, and an
    // origin in here would be a second, weaker source of truth about it.
    expect(target.get(REQUEST_PATH_HEADER)).toBe("/app?tab=inbox");
  });

  it("replaces rather than appends", () => {
    const target = new Headers({ [REQUEST_PATH_HEADER]: "/forged" });

    setRequestPath(target, new URL("http://localhost:3000/app"));

    expect(target.get(REQUEST_PATH_HEADER)).toBe("/app");
  });
});

describe("reading the path back", () => {
  it("returns what the proxy wrote", async () => {
    requestHeaders = new Headers({ [REQUEST_PATH_HEADER]: "/app?tab=inbox" });

    expect(await currentRequestPath()).toBe("/app?tab=inbox");
  });

  it("launders it, so a forged header cannot become an off-site redirect", async () => {
    // Belt to the proxy's braces: even if the strip were removed, this is the
    // only way the value reaches a navigation.
    for (const forged of ["https://evil.example", "//evil.example", "/\\evil.example"]) {
      requestHeaders = new Headers({ [REQUEST_PATH_HEADER]: forged });

      expect(await currentRequestPath()).toBe("/app");
    }
  });

  it("falls back to the default when there is no header at all", async () => {
    expect(await currentRequestPath()).toBe("/app");
  });

  it("does not read the path from any other header", async () => {
    requestHeaders = new Headers({ "x-forwarded-uri": "/somewhere" });

    expect(await currentRequestPath()).toBe("/app");
  });
});

describe("requireSession", () => {
  it("returns the session when there is one", async () => {
    signedIn();

    const session = await requireSession();

    expect(session.userId).toBe("u1");
    expect(session.organization.name).toBe("Acme");
    expect(redirected).toEqual([]);
  });

  it("sends an unauthenticated visitor to sign in, remembering where they were going", async () => {
    requestHeaders = new Headers({ [REQUEST_PATH_HEADER]: "/app?tab=inbox" });

    await expect(requireSession()).rejects.toThrow(/NEXT_REDIRECT/);
    expect(redirected).toEqual(["/login?next=%2Fapp%3Ftab%3Dinbox"]);
  });

  it("omits the parameter when the destination is where sign-in lands anyway", async () => {
    requestHeaders = new Headers({ [REQUEST_PATH_HEADER]: "/app" });

    await expect(requireSession()).rejects.toThrow(/NEXT_REDIRECT/);
    expect(redirected).toEqual(["/login"]);
  });

  it("never sends a visitor to an off-site destination", async () => {
    requestHeaders = new Headers({ [REQUEST_PATH_HEADER]: "https://evil.example/harvest" });

    await expect(requireSession()).rejects.toThrow(/NEXT_REDIRECT/);
    expect(redirected).toEqual(["/login"]);
  });

  it("treats a session cookie with no access token as no session", async () => {
    cookieJar.set(
      SESSION_COOKIE,
      encodeMetadata({
        userId: "u1",
        organization: { id: "o1", name: "Acme", slug: "acme", role: "owner" },
        accessExpiresAt: Date.now() + 600_000,
      }),
    );

    await expect(requireSession()).rejects.toThrow(/NEXT_REDIRECT/);
  });
});
