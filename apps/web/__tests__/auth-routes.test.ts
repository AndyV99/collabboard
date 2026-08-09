/**
 * The Route Handlers that turn tokens into cookies, and the proxy that keeps
 * the browser away from the auth endpoints entirely.
 *
 * `next/headers` is mocked with a jar that records what was written, because
 * `cookies()` has no request context in a unit test. Everything else — the
 * requests, the API responses, the response bodies — is real.
 */

import { NextRequest } from "next/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

const jar = new Map<string, { value: string; options: Record<string, unknown> }>();

vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => {
      const entry = jar.get(name);

      return entry === undefined ? undefined : { name, value: entry.value };
    },
    set: (name: string, value: string, options: Record<string, unknown>) => {
      jar.set(name, { value, options });
    },
  }),
  headers: async () => new Headers(),
}));

const {
  ACCESS_COOKIE,
  REFRESH_COOKIE,
  SESSION_COOKIE,
  encodeMetadata,
  metadataFromTokens,
} = await import("@/lib/session/cookies");
const { __resetInFlightForTests } = await import("@/lib/session/refresh");
const { POST: login } = await import("@/app/api/auth/login/route");
const { POST: refresh } = await import("@/app/api/auth/refresh/route");
const { POST: logout } = await import("@/app/api/auth/logout/route");
const { GET: sessionRoute } = await import("@/app/api/auth/session/route");
const { GET: proxyGet, POST: proxyPost } = await import(
  "@/app/api/proxy/[...path]/route"
);

const REFRESH_TOKEN = "opaque-refresh-token-nobody-should-see";
const ACCESS_TOKEN = "header.payload.signature";

const SESSION_BODY = {
  token_type: "Bearer",
  access_token: ACCESS_TOKEN,
  expires_in: 900,
  refresh_token: REFRESH_TOKEN,
  user_id: "11111111-1111-4111-8111-111111111111",
  organization: { id: "org-1", name: "Acme", slug: "acme", role: "owner" },
};

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** A same-origin POST, the way a browser sends one. */
function post(path: string, body?: unknown, headers: Record<string, string> = {}) {
  return new NextRequest(`http://localhost:3000${path}`, {
    method: "POST",
    headers: { "sec-fetch-site": "same-origin", ...headers },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

function get(path: string, headers: Record<string, string> = {}) {
  return new NextRequest(`http://localhost:3000${path}`, { headers });
}

function seedSession() {
  jar.set(ACCESS_COOKIE, { value: ACCESS_TOKEN, options: {} });
  jar.set(REFRESH_COOKIE, { value: REFRESH_TOKEN, options: {} });
  jar.set(SESSION_COOKIE, {
    value: encodeMetadata(
      metadataFromTokens(
        {
          tokenType: "Bearer",
          accessToken: ACCESS_TOKEN,
          expiresIn: 900,
          refreshToken: REFRESH_TOKEN,
          userId: SESSION_BODY.user_id,
          organization: SESSION_BODY.organization,
        },
        Date.now(),
      ),
    ),
    options: {},
  });
}

beforeEach(() => {
  jar.clear();
  __resetInFlightForTests();
  vi.unstubAllGlobals();
});

describe("POST /api/auth/login", () => {
  it("puts the tokens in httpOnly cookies and returns neither of them", async () => {
    vi.stubGlobal("fetch", (async () => json(SESSION_BODY)) as typeof fetch);

    const response = await login(post("/api/auth/login", { email: "a@b.c", password: "x" }));
    const body = await response.text();

    expect(response.status).toBe(200);

    // The load-bearing assertion of this whole PR: no token in the body the
    // browser can read.
    expect(body).not.toContain(REFRESH_TOKEN);
    expect(body).not.toContain(ACCESS_TOKEN);
    expect(JSON.parse(body)).toEqual({
      user_id: SESSION_BODY.user_id,
      organization: SESSION_BODY.organization,
    });

    expect(jar.get(REFRESH_COOKIE)?.value).toBe(REFRESH_TOKEN);
    expect(jar.get(REFRESH_COOKIE)?.options.httpOnly).toBe(true);
    expect(jar.get(ACCESS_COOKIE)?.options.httpOnly).toBe(true);
    expect(jar.get(SESSION_COOKIE)?.options.httpOnly).toBe(true);
    expect(response.headers.get("cache-control")).toBe("no-store");
  });

  it("relays the API's status so the UI can tell 401 from 429", async () => {
    for (const [status, body] of [
      [401, { error: "invalid email or password" }],
      [429, { error: "too many attempts, try again later" }],
    ] as const) {
      vi.stubGlobal("fetch", (async () => json(body, status)) as typeof fetch);

      const response = await login(
        post("/api/auth/login", { email: "a@b.c", password: "x" }),
      );

      expect(response.status).toBe(status);
      expect(await response.json()).toEqual(body);
      expect(jar.size).toBe(0);
    }
  });

  it("rejects a request that did not come from this site", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const response = await login(
      new NextRequest("http://localhost:3000/api/auth/login", {
        method: "POST",
        headers: { "sec-fetch-site": "cross-site" },
        body: JSON.stringify({ email: "a@b.c", password: "x" }),
      }),
    );

    expect(response.status).toBe(403);
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("rejects an incomplete body without spending a rate-limit slot", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    expect((await login(post("/api/auth/login", { email: "a@b.c" }))).status).toBe(400);
    expect((await login(post("/api/auth/login"))).status).toBe(400);
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});

describe("POST /api/auth/refresh", () => {
  it("takes no body: the token comes from the cookie the caller cannot read", async () => {
    seedSession();

    const sent: unknown[] = [];

    vi.stubGlobal("fetch", (async (_url: string, init: RequestInit) => {
      sent.push(JSON.parse(init.body as string));

      return json({ ...SESSION_BODY, refresh_token: "rotated", access_token: "new" });
    }) as unknown as typeof fetch);

    // A caller trying to supply its own token gets it ignored.
    const response = await refresh(
      post("/api/auth/refresh", { refresh_token: "attacker-chosen" }),
    );

    expect(response.status).toBe(204);
    expect(sent).toEqual([{ refresh_token: REFRESH_TOKEN }]);
    expect(jar.get(REFRESH_COOKIE)?.value).toBe("rotated");
  });

  it("clears the cookies and answers 401 when the session is over", async () => {
    seedSession();

    vi.stubGlobal(
      "fetch",
      (async () => json({ error: "session is no longer valid" }, 401)) as typeof fetch,
    );

    const response = await refresh(post("/api/auth/refresh"));

    expect(response.status).toBe(401);
    // Cleared, so the next request carries nothing and cannot come back here.
    // That is what stops a failed refresh becoming a loop.
    expect(jar.get(REFRESH_COOKIE)?.value).toBe("");
    expect(jar.get(REFRESH_COOKIE)?.options.maxAge).toBe(0);
    expect(jar.get(ACCESS_COOKIE)?.value).toBe("");
    expect(jar.get(SESSION_COOKIE)?.value).toBe("");
  });

  it("makes no API call at all once the cookies are gone", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const response = await refresh(post("/api/auth/refresh"));

    expect(response.status).toBe(401);
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("leaves the session alone when the API cannot be reached", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () => {
      throw new TypeError("fetch failed");
    }) as typeof fetch);

    const response = await refresh(post("/api/auth/refresh"));

    expect(response.status).toBe(502);
    expect(jar.get(REFRESH_COOKIE)?.value).toBe(REFRESH_TOKEN);
  });

  it("de-duplicates concurrent refreshes into one API call", async () => {
    seedSession();

    let calls = 0;

    vi.stubGlobal("fetch", (async () => {
      calls += 1;
      await new Promise((resolve) => setTimeout(resolve, 5));

      return json({ ...SESSION_BODY, refresh_token: "rotated" });
    }) as unknown as typeof fetch);

    const responses = await Promise.all([
      refresh(post("/api/auth/refresh")),
      refresh(post("/api/auth/refresh")),
      refresh(post("/api/auth/refresh")),
    ]);

    expect(calls).toBe(1);
    expect(responses.map((response) => response.status)).toEqual([204, 204, 204]);
  });
});

describe("POST /api/auth/logout", () => {
  it("revokes the session and clears the cookies", async () => {
    seedSession();

    const sent: unknown[] = [];

    vi.stubGlobal("fetch", (async (url: string, init: RequestInit) => {
      sent.push([url, JSON.parse(init.body as string)]);

      return new Response(null, { status: 204 });
    }) as unknown as typeof fetch);

    const response = await logout(post("/api/auth/logout"));

    expect(response.status).toBe(204);
    expect(sent).toEqual([
      ["http://localhost:8080/api/v1/auth/logout", { refresh_token: REFRESH_TOKEN }],
    ]);
    expect(jar.get(REFRESH_COOKIE)?.value).toBe("");
  });

  it("clears the cookies even when the API cannot be reached", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () => {
      throw new TypeError("fetch failed");
    }) as typeof fetch);

    const response = await logout(post("/api/auth/logout"));

    expect(response.status).toBe(204);
    // A logout that leaves a live cookie behind because the network failed is a
    // logout that did not happen, and the user has been told it did.
    expect(jar.get(REFRESH_COOKIE)?.value).toBe("");
  });
});

describe("GET /api/auth/session", () => {
  it("reports who is signed in, without any token", async () => {
    seedSession();

    const response = await sessionRoute();
    const body = await response.text();

    expect(response.status).toBe(200);
    expect(body).not.toContain(REFRESH_TOKEN);
    expect(body).not.toContain(ACCESS_TOKEN);
    expect(JSON.parse(body).organization).toEqual(SESSION_BODY.organization);
  });

  it("answers 401 when there is no session", async () => {
    expect((await sessionRoute()).status).toBe(401);
  });
});

describe("/api/proxy", () => {
  const context = (path: string[]) => ({ params: Promise.resolve({ path }) });

  it("forwards an allowed path with the bearer token from the cookie", async () => {
    seedSession();

    const seen: { url: string; auth: string | undefined }[] = [];

    vi.stubGlobal("fetch", (async (url: string, init: RequestInit) => {
      seen.push({
        url,
        auth: (init.headers as Record<string, string>).authorization,
      });

      return json({ projects: [] });
    }) as unknown as typeof fetch);

    const response = await proxyGet(get("/api/proxy/projects"), context(["projects"]));

    expect(response.status).toBe(200);
    expect(seen).toEqual([
      {
        url: "http://localhost:8080/api/v1/projects",
        auth: `Bearer ${ACCESS_TOKEN}`,
      },
    ]);
  });

  it("REFUSES the auth prefix, so a browser cannot ask for a refresh token", async () => {
    seedSession();

    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    for (const path of [["auth", "login"], ["auth", "refresh"], ["auth", "logout"]]) {
      const response = await proxyPost(
        post(`/api/proxy/${path.join("/")}`, { email: "a", password: "b" }),
        context(path),
      );

      expect(response.status).toBe(404);
    }

    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("refuses anything not on the allowlist, including the websocket upgrade", async () => {
    seedSession();

    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    for (const path of [["ws"], ["healthz"], [".."], ["admin"]]) {
      expect((await proxyGet(get("/api/proxy/x"), context(path))).status).toBe(404);
    }

    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("refuses a cross-site write", async () => {
    seedSession();

    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const response = await proxyPost(
      new NextRequest("http://localhost:3000/api/proxy/projects", {
        method: "POST",
        headers: { "sec-fetch-site": "cross-site" },
        body: JSON.stringify({ name: "x" }),
      }),
      context(["projects"]),
    );

    expect(response.status).toBe(403);
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("refreshes and retries once on a 401, then relays the answer", async () => {
    seedSession();
    jar.set(ACCESS_COOKIE, { value: "expired", options: {} });

    const urls: string[] = [];

    vi.stubGlobal("fetch", (async (url: string, init: RequestInit) => {
      urls.push(url);

      if (url.endsWith("/auth/refresh")) {
        return json({ ...SESSION_BODY, access_token: "fresh", refresh_token: "rotated" });
      }

      return (init.headers as Record<string, string>).authorization === "Bearer fresh"
        ? json({ projects: [] })
        : json({ error: "authentication required" }, 401);
    }) as unknown as typeof fetch);

    const response = await proxyGet(get("/api/proxy/projects"), context(["projects"]));

    expect(response.status).toBe(200);
    expect(urls.filter((url) => url.endsWith("/auth/refresh"))).toHaveLength(1);
    expect(urls).toHaveLength(3);
    // The rotated refresh token was persisted, so the cookie does not now hold
    // one the API would treat as a replay.
    expect(jar.get(REFRESH_COOKIE)?.value).toBe("rotated");
  });

  it("relays a 409 and a 429 with its Retry-After intact", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () =>
      new Response(JSON.stringify({ error: "too many attempts, try again later" }), {
        status: 429,
        headers: { "content-type": "application/json", "retry-after": "37" },
      })) as unknown as typeof fetch);

    const response = await proxyGet(get("/api/proxy/projects"), context(["projects"]));

    expect(response.status).toBe(429);
    expect(response.headers.get("retry-after")).toBe("37");

    vi.stubGlobal("fetch", (async () =>
      json({ error: "card is not on that board" }, 409)) as typeof fetch);

    const conflict = await proxyGet(get("/api/proxy/cards/c1"), context(["cards", "c1"]));

    expect(conflict.status).toBe(409);
    expect(await conflict.json()).toEqual({ error: "card is not on that board" });
  });

  it("relays a 204 as a 204", async () => {
    seedSession();

    vi.stubGlobal(
      "fetch",
      (async () => new Response(null, { status: 204 })) as typeof fetch,
    );

    const response = await proxyGet(get("/api/proxy/cards/c1"), context(["cards", "c1"]));

    expect(response.status).toBe(204);
  });

  it("never caches a relayed response", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () => json({ projects: [] })) as typeof fetch);

    const response = await proxyGet(get("/api/proxy/projects"), context(["projects"]));

    expect(response.headers.get("cache-control")).toBe("no-store");
  });
});
