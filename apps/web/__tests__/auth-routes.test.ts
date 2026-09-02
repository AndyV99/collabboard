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

/**
 * The request headers a handler sees.
 *
 * Controllable on purpose. The original version of this mock always returned an
 * empty `Headers`, which is structurally why nothing noticed that a client could
 * forge `x-collabboard-session` and be believed — see the forged-header tests
 * below.
 */
let requestHeaders = new Headers();

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
  headers: async () => requestHeaders,
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
const { __resetInFlightForTests } = await import("@/lib/session/refresh");
const { POST: login } = await import("@/app/api/auth/login/route");
const { POST: register } = await import("@/app/api/auth/register/route");
const { POST: refresh } = await import("@/app/api/auth/refresh/route");
const { POST: logout } = await import("@/app/api/auth/logout/route");
const { GET: sessionRoute } = await import("@/app/api/auth/session/route");
const { POST: switchOrg } = await import("@/app/api/auth/organization/route");
const { POST: firstOrganization } = await import(
  "@/app/api/auth/first-organization/route"
);
const { GET: proxyGet, POST: proxyPost } = await import(
  "@/app/api/proxy/[...path]/route"
);
const proxyModule = await import("@/proxy");

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
  requestHeaders = new Headers();
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

    // The Go API also answers 403 — "this account does not belong to an
    // organization", which is a completely different screen — so the sign-in
    // form has to tell the two apart by something other than the status. This
    // header is that something; `relayApiError` never sets it.
    expect(response.headers.get("x-collabboard-refusal")).toBe("cross-origin");
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

  it("relays a 201 as a 201, not as a 200", async () => {
    /*
     * The module comment promises "the API's own status", and until #80 that
     * was true of failures and false of successes: `Response.json` defaults to
     * 200, so every create route on the Go side — which all answer 201 — was
     * quietly flattened.
     *
     * Nothing in `apps/web` branched on a success status, so this never broke
     * anything. It is worth fixing because the file claimed otherwise and
     * because the next endpoint to answer 202 would have been rewritten too.
     */
    seedSession();

    vi.stubGlobal(
      "fetch",
      (async () => json({ project: { id: "p1" } }, 201)) as typeof fetch,
    );

    const response = await proxyPost(
      post("/api/proxy/projects", { name: "Launch" }),
      context(["projects"]),
    );

    expect(response.status).toBe(201);
    expect(await response.json()).toEqual({ project: { id: "p1" } });
  });

  it("relays a 200 as a 200, so the fix did not just move the constant", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () => json({ projects: [] })) as typeof fetch);

    const response = await proxyGet(get("/api/proxy/projects"), context(["projects"]));

    expect(response.status).toBe(200);
  });

  it("relays a 205 without rewriting it into a 204", async () => {
    // Both empty-bodied statuses go through `expectNoContent`, so a hard-coded
    // 204 in that branch would silently change the meaning of the other one.
    // No route answers 205 today; that is the point of asserting it.
    seedSession();

    vi.stubGlobal(
      "fetch",
      (async () => new Response(null, { status: 205 })) as typeof fetch,
    );

    const response = await proxyGet(get("/api/proxy/cards/c1"), context(["cards", "c1"]));

    expect(response.status).toBe(205);
    expect(await response.text()).toBe("");
  });

  it("never caches a relayed response", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () => json({ projects: [] })) as typeof fetch);

    const response = await proxyGet(get("/api/proxy/projects"), context(["projects"]));

    expect(response.headers.get("cache-control")).toBe("no-store");
  });

  it("cannot be walked out of an allowed root with a dot segment", async () => {
    // The end-to-end form of the traversal: an allowlisted first segment, then
    // "..", resolving to POST /auth/organization — which answers with a refresh
    // token in its body. Asserted on the URL that would actually leave, not on
    // the reason code.
    seedSession();

    const urls: string[] = [];

    vi.stubGlobal("fetch", (async (url: string) => {
      urls.push(new URL(url).pathname);

      return json({ ok: true });
    }) as unknown as typeof fetch);

    const attempts = [
      ["projects", "..", "auth", "organization"],
      ["cards", "..", "auth", "login"],
      ["boards", "..", "auth", "refresh"],
    ];

    for (const path of attempts) {
      const response = await proxyPost(
        post(`/api/proxy/${path.join("/")}`, { organization_id: "o1" }),
        context(path),
      );

      expect(response.status).toBe(404);
    }

    expect(urls).toEqual([]);
  });

  it("relays an unusable 200 as a 502 rather than a 200 with an error body", async () => {
    // `malformed` keeps the upstream status, and the upstream status here is a
    // success — a gateway's HTML page, or API_URL pointing at another service.
    // Relaying it verbatim would produce `200 {"error": ...}`, which every
    // client in this repo reads as a success.
    seedSession();

    vi.stubGlobal("fetch", (async () =>
      new Response("<html>hello</html>", {
        status: 200,
        headers: { "content-type": "text/html" },
      })) as typeof fetch);

    const response = await proxyGet(get("/api/proxy/projects"), context(["projects"]));

    expect(response.status).toBe(502);
  });
});

describe("the forwarded session header is not a credential", () => {
  const context = (path: string[]) => ({ params: Promise.resolve({ path }) });

  function forged(): string {
    return encodeForwardedSession({
      accessToken: "a-leaked-15-minute-access-token",
      metadata: {
        userId: "attacker",
        organization: { id: "o1", name: "n", slug: "s", role: "owner" },
        accessExpiresAt: Date.now() + 600_000,
      },
    });
  }

  it("does not let a forged header mint a session at /api/auth/organization", async () => {
    // The escalation this closes: POST /auth/organization answers with a *new*
    // 14-day refresh token, and the handler writes it into a cookie. Trusting an
    // unsigned request header here turned any leaked access token into a
    // long-lived cookie session.
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);
    requestHeaders = new Headers({ [FORWARDED_SESSION_HEADER]: forged() });

    const response = await switchOrg(
      post("/api/auth/organization", { organization_id: "o1" }),
    );

    expect(response.status).toBe(401);
    expect(fetchImpl).not.toHaveBeenCalled();
    expect(jar.size).toBe(0);
  });

  it("does not let a forged header authenticate a proxied request", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);
    requestHeaders = new Headers({ [FORWARDED_SESSION_HEADER]: forged() });

    const response = await proxyGet(get("/api/proxy/me"), context(["me"]));

    expect(response.status).toBe(401);
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("is stripped before it reaches any handler, because proxy.ts runs on /api/*", () => {
    // The second, independent control. A path proxy.ts does not run on is a path
    // where a client can supply its own header, so the matcher itself is part of
    // the security property and is asserted as one.
    const pattern = new RegExp(`^${proxyModule.config.matcher[0]}$`);

    expect(pattern.test("/api/auth/organization")).toBe(true);
    expect(pattern.test("/api/proxy/me")).toBe(true);
    expect(pattern.test("/boards/b1")).toBe(true);
    expect(pattern.test("/_next/static/chunk.js")).toBe(false);
  });
});

describe("POST /api/auth/register", () => {
  it("relays the created account and starts no session", async () => {
    vi.stubGlobal("fetch", (async () =>
      json(
        {
          user_id: "u1",
          email: "a@b.c",
          display_name: "A",
          organization: SESSION_BODY.organization,
        },
        201,
      )) as typeof fetch);

    const response = await register(
      post("/api/auth/register", {
        email: "a@b.c",
        password: "x",
        display_name: "A",
      }),
    );

    expect(response.status).toBe(201);
    // Registration does not log anyone in — the API returns no tokens, and this
    // handler does not invent a session from the response.
    expect(jar.size).toBe(0);
  });

  it("relays a duplicate address as a 409", async () => {
    vi.stubGlobal("fetch", (async () =>
      json({ error: "email is already registered" }, 409)) as typeof fetch);

    const response = await register(
      post("/api/auth/register", { email: "a@b.c", password: "x", display_name: "A" }),
    );

    expect(response.status).toBe(409);
  });

  it("rejects an incomplete body without a round trip", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    expect(
      (await register(post("/api/auth/register", { email: "a@b.c", password: "x" })))
        .status,
    ).toBe(400);
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});

describe("POST /api/auth/first-organization", () => {
  const CREATED_BODY = {
    user_id: SESSION_BODY.user_id,
    organization: { id: "org-9", name: "Acme", slug: "acme", role: "owner" },
  };

  it("relays the 201 and writes no cookies, because this is not a session", async () => {
    // The invariant ADR 0009 rests on: `POST /organizations` answers with the
    // organization and no tokens, so the recovery path mints nothing. A session
    // still comes only from presenting a password to `/api/auth/login`, which
    // is the very next request the form makes.
    vi.stubGlobal("fetch", (async () => json(CREATED_BODY, 201)) as typeof fetch);

    const response = await firstOrganization(
      post("/api/auth/first-organization", { email: "a@b.c", password: "x" }),
    );

    expect(response.status).toBe(201);
    expect(await response.json()).toEqual(CREATED_BODY);
    expect(jar.size).toBe(0);
    expect(response.headers.get("cache-control")).toBe("no-store");
  });

  it("forwards the credentials and the chosen name, and omits an absent name", async () => {
    const bodies: unknown[] = [];
    const fetchImpl = vi.fn(async (_url: string, init: RequestInit) => {
      bodies.push(JSON.parse(String(init.body)));

      return json(CREATED_BODY, 201);
    });

    vi.stubGlobal("fetch", fetchImpl as unknown as typeof fetch);

    await firstOrganization(
      post("/api/auth/first-organization", {
        email: "a@b.c",
        password: "x",
        organization_name: "Acme",
      }),
    );
    await firstOrganization(
      post("/api/auth/first-organization", { email: "a@b.c", password: "x" }),
    );

    expect(bodies[0]).toEqual({ email: "a@b.c", password: "x", organization_name: "Acme" });
    expect(bodies[1]).toEqual({ email: "a@b.c", password: "x" });
  });

  it("relays the statuses the recovery screen branches on", async () => {
    // 409 drives a notice rather than an error, and 429 carries the wait. Both
    // are normal answers here, so both have to survive the relay intact.
    for (const [status, body, retryAfter] of [
      [401, { error: "invalid email or password" }, undefined],
      [409, { error: "this account already belongs to an organization" }, undefined],
      [429, { error: "too many attempts, try again later" }, "900"],
    ] as const) {
      vi.stubGlobal(
        "fetch",
        (async () =>
          new Response(JSON.stringify(body), {
            status,
            headers: {
              "content-type": "application/json",
              ...(retryAfter === undefined ? {} : { "retry-after": retryAfter }),
            },
          })) as typeof fetch,
      );

      const response = await firstOrganization(
        post("/api/auth/first-organization", { email: "a@b.c", password: "x" }),
      );

      expect(response.status).toBe(status);
      expect(await response.json()).toEqual(body);
      expect(jar.size).toBe(0);
    }
  });

  it("carries Retry-After through, because the form disables itself on it", async () => {
    vi.stubGlobal(
      "fetch",
      (async () =>
        new Response(JSON.stringify({ error: "too many attempts, try again later" }), {
          status: 429,
          headers: { "content-type": "application/json", "retry-after": "900" },
        })) as typeof fetch,
    );

    const response = await firstOrganization(
      post("/api/auth/first-organization", { email: "a@b.c", password: "x" }),
    );

    expect(response.headers.get("retry-after")).toBe("900");
  });

  it("rejects a request that did not come from this site, and marks the refusal", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const response = await firstOrganization(
      new NextRequest("http://localhost:3000/api/auth/first-organization", {
        method: "POST",
        headers: { "sec-fetch-site": "cross-site" },
        body: JSON.stringify({ email: "a@b.c", password: "x" }),
      }),
    );

    expect(response.status).toBe(403);
    expect(fetchImpl).not.toHaveBeenCalled();

    // Load-bearing on this route too, and in the opposite direction from login:
    // the Go API cannot answer 403 here at all, so `describeFirstOrganization\
    // Failure` reads a *marked* 403 as a CSRF refusal and an unmarked one as
    // something it does not understand. Drop this header and a refused
    // cross-origin post starts telling the user the server is unreachable.
    expect(response.headers.get("x-collabboard-refusal")).toBe("cross-origin");
  });

  it("rejects an incomplete body without spending a rate-limit slot", async () => {
    // This route is charged against the sign-in budget *before* the credential
    // is checked, so a body the API would reject out of hand still costs a real
    // attempt. Cheaper and kinder to refuse it here.
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const bad = [
      { email: "a@b.c" },
      { password: "x" },
      { email: 1, password: "x" },
      { email: "a@b.c", password: "x", organization_name: 7 },
      undefined,
    ];

    for (const body of bad) {
      const response = await firstOrganization(post("/api/auth/first-organization", body));

      expect(response.status).toBe(400);
    }

    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("never writes the address or the password into a log line", async () => {
    // The handler's comment promises this, and a log keyed by email is an
    // enumeration oracle for anyone who can read logs. Asserted rather than
    // trusted, because "let's add the email for debugging" is a one-line edit.
    const lines: string[] = [];
    const capture = (line: string) => {
      lines.push(line);
    };

    vi.spyOn(console, "info").mockImplementation(capture);
    vi.spyOn(console, "warn").mockImplementation(capture);
    vi.spyOn(console, "error").mockImplementation(capture);

    try {
      vi.stubGlobal(
        "fetch",
        (async () => json({ error: "invalid email or password" }, 401)) as typeof fetch,
      );
      await firstOrganization(
        post("/api/auth/first-organization", {
          email: "orphan@example.com",
          password: "correct horse battery",
        }),
      );

      vi.stubGlobal("fetch", (async () => json(CREATED_BODY, 201)) as typeof fetch);
      await firstOrganization(
        post("/api/auth/first-organization", {
          email: "orphan@example.com",
          password: "correct horse battery",
        }),
      );

      expect(lines.length).toBeGreaterThan(0);

      const logged = lines.join("\n");

      expect(logged).not.toContain("orphan@example.com");
      expect(logged).not.toContain("correct horse battery");
      expect(logged).not.toContain("password");
      // The successful one does identify the account — by id, which is not an
      // address and not a credential.
      expect(logged).toContain(SESSION_BODY.user_id);
    } finally {
      vi.restoreAllMocks();
    }
  });
});

describe("POST /api/auth/organization", () => {
  it("writes all three cookies, because the API issues a whole new session", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () =>
      json({
        ...SESSION_BODY,
        access_token: "new-access",
        refresh_token: "new-refresh",
      })) as typeof fetch);

    const response = await switchOrg(
      post("/api/auth/organization", { organization_id: "o2" }),
    );
    const body = await response.text();

    expect(response.status).toBe(200);
    expect(body).not.toContain("new-refresh");
    expect(jar.get(REFRESH_COOKIE)?.value).toBe("new-refresh");
    expect(jar.get(ACCESS_COOKIE)?.value).toBe("new-access");
    expect(jar.get(SESSION_COOKIE)?.options.httpOnly).toBe(true);
  });

  it("relays a non-member as a 403 and changes nothing", async () => {
    seedSession();

    vi.stubGlobal("fetch", (async () =>
      json({ error: "not a member of that organization" }, 403)) as typeof fetch);

    const response = await switchOrg(
      post("/api/auth/organization", { organization_id: "someone-elses" }),
    );

    expect(response.status).toBe(403);
    expect(jar.get(REFRESH_COOKIE)?.value).toBe(REFRESH_TOKEN);
  });

  it("refuses a cross-site request", async () => {
    seedSession();

    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);

    const response = await switchOrg(
      new NextRequest("http://localhost:3000/api/auth/organization", {
        method: "POST",
        headers: { "sec-fetch-site": "cross-site" },
        body: JSON.stringify({ organization_id: "o2" }),
      }),
    );

    expect(response.status).toBe(403);
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});
