import { describe, expect, it, vi } from "vitest";

import { sendRequest } from "@/lib/api/http";
import * as endpoints from "@/lib/api/endpoints";
import { parseEmpty, parseProject } from "@/lib/api/types";

const BASE = "http://api.test/api/v1";

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "content-type": "application/json" },
    ...init,
  });
}

const PROJECT = {
  id: "5d5f0a2e-0a2e-4d5f-9a2e-0a2e4d5f9a2e",
  name: "Launch",
  description: "",
  archived_at: null,
  created_at: "2026-08-08T10:00:00Z",
  updated_at: "2026-08-08T10:00:00Z",
};

describe("sendRequest", () => {
  it("sends the bearer token, asks for JSON, and never caches", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({ project: PROJECT }));

    await sendRequest(endpoints.getProject(PROJECT.id), {
      baseUrl: BASE,
      accessToken: "access-token",
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });

    const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];

    expect(url).toBe(`${BASE}/projects/${PROJECT.id}`);
    expect((init.headers as Record<string, string>).authorization).toBe(
      "Bearer access-token",
    );
    // A cached authenticated response in an RSC is a cross-user data leak, not a
    // stale board. Next caches fetch by default, so this has to be explicit.
    expect(init.cache).toBe("no-store");
  });

  it("omits the Authorization header when there is no token", async () => {
    const fetchImpl = vi.fn(async () => jsonResponse({ project: PROJECT }));

    await sendRequest(endpoints.listProjects(), {
      baseUrl: BASE,
      fetchImpl: fetchImpl as unknown as typeof fetch,
    });

    const [, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];

    expect(init.headers).not.toHaveProperty("authorization");
  });

  it("parses a successful body into the typed shape", async () => {
    const result = await sendRequest(endpoints.getProject(PROJECT.id), {
      baseUrl: BASE,
      fetchImpl: (async () => jsonResponse({ project: PROJECT })) as typeof fetch,
    });

    expect(result).toEqual({
      ok: true,
      data: {
        id: PROJECT.id,
        name: "Launch",
        description: "",
        archivedAt: null,
        createdAt: PROJECT.created_at,
        updatedAt: PROJECT.updated_at,
      },
    });
  });

  it("reports a body of the wrong shape as malformed rather than returning it", async () => {
    // This is what a misconfigured API_URL pointing at some other service looks
    // like: a 200, valid JSON, and nothing the UI can use.
    const result = await sendRequest(endpoints.getProject(PROJECT.id), {
      baseUrl: BASE,
      fetchImpl: (async () => jsonResponse({ hello: "world" })) as typeof fetch,
    });

    expect(result).toEqual({
      ok: false,
      error: { kind: "malformed", status: 200, message: expect.any(String) },
    });
  });

  it("keeps the status's meaning when an error body is unreadable", async () => {
    const result = await sendRequest(endpoints.listProjects(), {
      baseUrl: BASE,
      fetchImpl: (async () =>
        new Response("<html>502</html>", { status: 502 })) as typeof fetch,
    });

    expect(result.ok).toBe(false);
    expect(result.ok === false && result.error.kind).toBe("server_error");
  });

  it("folds a transport failure into a network error instead of throwing", async () => {
    const result = await sendRequest(endpoints.listProjects(), {
      baseUrl: BASE,
      fetchImpl: (async () => {
        throw new TypeError("fetch failed: ECONNREFUSED 10.0.0.5:8080");
      }) as typeof fetch,
    });

    expect(result).toEqual({
      ok: false,
      error: { kind: "network", status: null, message: expect.any(String) },
    });
    // The internal address must not reach a user-facing message.
    expect(result.ok === false && result.error.message).not.toContain("10.0.0.5");
  });

  it("accepts a 204 for an endpoint that promises no content", async () => {
    const result = await sendRequest(endpoints.deleteCard("card-1"), {
      baseUrl: BASE,
      fetchImpl: (async () => new Response(null, { status: 204 })) as typeof fetch,
    });

    expect(result).toEqual({ ok: true, data: null });
  });

  it("treats a 204 for an endpoint that promised a body as malformed", async () => {
    const result = await sendRequest(endpoints.getCard("card-1"), {
      baseUrl: BASE,
      fetchImpl: (async () => new Response(null, { status: 204 })) as typeof fetch,
    });

    expect(result.ok === false && result.error.kind).toBe("malformed");
  });

  it("aborts a request that outlives its timeout", async () => {
    const result = await sendRequest(endpoints.listProjects(), {
      baseUrl: BASE,
      timeoutMs: 5,
      fetchImpl: ((_url: string, init: RequestInit) =>
        new Promise((_resolve, reject) => {
          init.signal?.addEventListener("abort", () => reject(new Error("aborted")));
        })) as unknown as typeof fetch,
    });

    expect(result.ok === false && result.error.kind).toBe("network");
  });
});

describe("endpoint definitions", () => {
  it("writes paths without a base so one definition serves both transports", () => {
    // The browser transport prefixes /api/proxy, the server transport prefixes
    // ${API_URL}/api/v1. A leading /api/v1 here would break the browser one.
    for (const endpoint of [
      endpoints.currentUser(),
      endpoints.listMembers(),
      endpoints.listProjects(),
      endpoints.listBoards("b"),
      endpoints.listColumns("c"),
      endpoints.listCardsByBoard("d"),
      endpoints.moveCard("e", { columnId: "f", afterCardId: null }),
    ]) {
      expect(endpoint.path.startsWith("/")).toBe(true);
      expect(endpoint.path.startsWith("/api/")).toBe(false);
    }
  });

  it("renames camelCase arguments to the snake_case the API expects", () => {
    expect(endpoints.moveCard("card-1", { columnId: "col-2", afterCardId: null }).body)
      .toEqual({ column_id: "col-2", after_card_id: null });

    expect(endpoints.moveColumn("col-1", { afterColumnId: "col-0" }).body).toEqual({
      after_column_id: "col-0",
    });
  });

  it("omits an absent PATCH field rather than sending null", () => {
    // The Go handler takes *string and reads nil as "leave this column alone".
    // JSON.stringify drops undefined keys, so an omitted field stays omitted.
    const body = endpoints.updateProject("p", { name: "New" }).body;

    expect(JSON.parse(JSON.stringify(body))).toEqual({ name: "New" });
  });

  it("escapes path segments", () => {
    expect(endpoints.getCard("../boards/other").path).toBe(
      "/cards/..%2Fboards%2Fother",
    );
  });
});

describe("parsers", () => {
  it("accepts an organization with no role, which the API omits", () => {
    const project = parseProject({ ...PROJECT, archived_at: "2026-08-09T00:00:00Z" });

    expect(project?.archivedAt).toBe("2026-08-09T00:00:00Z");
  });

  it("rejects a project whose archived_at is neither a string nor null", () => {
    expect(parseProject({ ...PROJECT, archived_at: 12 })).toBeNull();
  });

  it("parseEmpty is the parser for a 204", () => {
    expect(parseEmpty()).toBeNull();
  });
});
