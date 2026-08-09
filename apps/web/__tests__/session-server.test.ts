/**
 * `getServerSession`, and the Server Component half of the boundary.
 *
 * The behaviour that matters here is precedence: on the request where `proxy.ts`
 * just refreshed, the cookies still say the *old* token — the new ones are on a
 * response the browser has not seen yet — so the forwarded header has to win.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

const state = {
  cookies: new Map<string, string>(),
  headers: new Headers(),
  writes: [] as { name: string; value: string }[],
};

vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => {
      const value = state.cookies.get(name);

      return value === undefined ? undefined : { name, value };
    },
    set: (name: string, value: string) => {
      state.writes.push({ name, value });
      state.cookies.set(name, value);
    },
  }),
  headers: async () => state.headers,
}));

const {
  ACCESS_COOKIE,
  REFRESH_COOKIE,
  SESSION_COOKIE,
  encodeMetadata,
  metadataFromTokens,
} = await import("@/lib/session/cookies");
const { FORWARDED_SESSION_HEADER, encodeForwardedSession } = await import(
  "@/lib/session/forward"
);
const { getRefreshToken, getRenderSession, getServerSession } = await import(
  "@/lib/session/server"
);
const { mutableServerApi, serverApi } = await import("@/lib/api/server");
const { __resetInFlightForTests } = await import("@/lib/session/refresh");
const endpoints = await import("@/lib/api/endpoints");

const ORG = { id: "o1", name: "Acme", slug: "acme", role: "owner" };

function tokens(accessToken: string) {
  return {
    tokenType: "Bearer",
    accessToken,
    expiresIn: 900,
    refreshToken: "rt",
    userId: "u1",
    organization: ORG,
  };
}

beforeEach(() => {
  state.cookies.clear();
  state.headers = new Headers();
  state.writes = [];
  __resetInFlightForTests();
  vi.unstubAllGlobals();
});

describe("getServerSession", () => {
  it("returns null when there is no session", async () => {
    expect(await getServerSession()).toBeNull();
  });

  it("reads the session from the cookies", async () => {
    state.cookies.set(ACCESS_COOKIE, "cookie-access");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));

    const session = await getServerSession();

    expect(session?.accessToken).toBe("cookie-access");
    expect(session?.organization).toEqual(ORG);
  });

  it("ignores the forwarded header, because a Route Handler can be sent one", async () => {
    // The header is unsigned — it proves shape, not provenance. Only the render
    // reader consults it, so a handler that reaches for the wrong function does
    // not silently accept a client's word for who it is.
    state.cookies.set(ACCESS_COOKIE, "cookie-access");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));
    state.headers.set(
      FORWARDED_SESSION_HEADER,
      encodeForwardedSession({
        accessToken: "forged",
        metadata: metadataFromTokens(tokens("forged")),
      }),
    );

    expect((await getServerSession())?.accessToken).toBe("cookie-access");
  });

  it("answers nothing at all when only a forged header is present", async () => {
    state.headers.set(
      FORWARDED_SESSION_HEADER,
      encodeForwardedSession({
        accessToken: "forged",
        metadata: metadataFromTokens(tokens("forged")),
      }),
    );

    expect(await getServerSession()).toBeNull();
  });
});

describe("getRenderSession", () => {
  it("prefers the token the proxy just minted over the stale cookie", async () => {
    // On a request where proxy.ts refreshed, the request's cookies are the old
    // ones — the new ones are on a response the browser has not seen yet.
    state.cookies.set(ACCESS_COOKIE, "stale-cookie-access");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));
    state.headers.set(
      FORWARDED_SESSION_HEADER,
      encodeForwardedSession({
        accessToken: "just-refreshed",
        metadata: metadataFromTokens(tokens("just-refreshed")),
      }),
    );

    expect((await getRenderSession())?.accessToken).toBe("just-refreshed");
  });

  it("falls back to the cookies when the proxy did not refresh", async () => {
    state.cookies.set(ACCESS_COOKIE, "cookie-access");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));

    expect((await getRenderSession())?.accessToken).toBe("cookie-access");
  });

  it("exposes no refresh token on the session type", async () => {
    state.cookies.set(ACCESS_COOKIE, "at");
    state.cookies.set(REFRESH_COOKIE, "the-refresh-token");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));

    const session = await getServerSession();

    expect(JSON.stringify(session)).not.toContain("the-refresh-token");
    // It is still reachable, but only through the one function named for it —
    // so "who can get a refresh token" is a grep.
    expect(await getRefreshToken()).toBe("the-refresh-token");
  });
});

describe("serverApi", () => {
  it("sends the session's access token to the API", async () => {
    state.cookies.set(ACCESS_COOKIE, "server-access");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));

    const seen: { url: string; auth: string | undefined }[] = [];

    vi.stubGlobal("fetch", (async (url: string, init: RequestInit) => {
      seen.push({ url, auth: (init.headers as Record<string, string>).authorization });

      return new Response(JSON.stringify({ projects: [] }), {
        headers: { "content-type": "application/json" },
      });
    }) as unknown as typeof fetch);

    const result = await serverApi(endpoints.listProjects());

    expect(result).toEqual({ ok: true, data: [] });
    expect(seen).toEqual([
      { url: "http://localhost:8080/api/v1/projects", auth: "Bearer server-access" },
    ]);
  });

  it("does not refresh on a 401, because it could not persist the result", async () => {
    // The Server Component rule. A refresh here would rotate the token and drop
    // the successor, costing the user their session for one render. `proxy.ts`
    // refreshes before the render so this path is only reached when the session
    // is genuinely gone.
    state.cookies.set(ACCESS_COOKIE, "expired");
    state.cookies.set(REFRESH_COOKIE, "perfectly-good-refresh-token");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));

    const urls: string[] = [];

    vi.stubGlobal("fetch", (async (url: string) => {
      urls.push(url);

      return new Response(JSON.stringify({ error: "authentication required" }), {
        status: 401,
        headers: { "content-type": "application/json" },
      });
    }) as unknown as typeof fetch);

    const result = await serverApi(endpoints.listProjects());

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(urls).toEqual(["http://localhost:8080/api/v1/projects"]);
  });

  it("answers 401 with no round trip when signed out", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const result = await serverApi(endpoints.currentUser());

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});

describe("mutableServerApi", () => {
  it("refreshes, persists the rotated tokens, and retries once", async () => {
    // The other half of the boundary: a Route Handler or Server Action can set
    // cookies, so it may spend the refresh token — because it can store the
    // successor.
    state.cookies.set(ACCESS_COOKIE, "expired");
    state.cookies.set(REFRESH_COOKIE, "rt");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));

    const urls: string[] = [];

    vi.stubGlobal("fetch", (async (url: string, init: RequestInit) => {
      urls.push(url);

      if (url.endsWith("/auth/refresh")) {
        return new Response(
          JSON.stringify({
            token_type: "Bearer",
            access_token: "fresh",
            expires_in: 900,
            refresh_token: "rotated",
            user_id: "u1",
            organization: ORG,
          }),
          { headers: { "content-type": "application/json" } },
        );
      }

      return (init.headers as Record<string, string>).authorization === "Bearer fresh"
        ? new Response(JSON.stringify({ members: [] }), {
            headers: { "content-type": "application/json" },
          })
        : new Response(JSON.stringify({ error: "authentication required" }), {
            status: 401,
            headers: { "content-type": "application/json" },
          });
    }) as unknown as typeof fetch);

    const result = await mutableServerApi(endpoints.listMembers());

    expect(result).toEqual({ ok: true, data: [] });
    expect(urls.filter((url) => url.endsWith("/auth/refresh"))).toHaveLength(1);
    expect(state.writes.map((write) => write.name)).toEqual([
      ACCESS_COOKIE,
      REFRESH_COOKIE,
      SESSION_COOKIE,
    ]);
    expect(state.cookies.get(REFRESH_COOKIE)).toBe("rotated");
  });

  it("clears the cookies when the refresh is rejected", async () => {
    state.cookies.set(ACCESS_COOKIE, "expired");
    state.cookies.set(REFRESH_COOKIE, "dead");
    state.cookies.set(SESSION_COOKIE, encodeMetadata(metadataFromTokens(tokens("x"))));

    vi.stubGlobal("fetch", (async () =>
      new Response(JSON.stringify({ error: "session is no longer valid" }), {
        status: 401,
        headers: { "content-type": "application/json" },
      })) as typeof fetch);

    const result = await mutableServerApi(endpoints.listMembers());

    expect(result.ok === false && result.error.kind).toBe("unauthorized");
    expect(state.cookies.get(REFRESH_COOKIE)).toBe("");
    expect(state.cookies.get(ACCESS_COOKIE)).toBe("");
  });
});
