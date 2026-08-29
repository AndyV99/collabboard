import { describe, expect, it, vi } from "vitest";

import { sendRequest } from "@/lib/api/http";
import * as endpoints from "@/lib/api/endpoints";
import { parseAddedMember, parseCard, parseEmpty, parseProject } from "@/lib/api/types";

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

const CARD = {
  id: "7c9e6679-7425-40de-944b-e07fc1f90ae7",
  board_id: "b1",
  column_id: "col1",
  title: "Ship it",
  description: "",
  assignee_id: null,
  due_at: null,
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

  it("distinguishes leaving a card field alone from clearing it", () => {
    /*
     * The three states `PATCH /cards/:id` reads through `Optional[string]`, and
     * the reason `updateCard` builds its body key by key instead of spreading
     * its argument. `JSON.stringify` is applied here because that is what the
     * transport does: a key whose value is `undefined` does not survive it, so
     * asserting on the object alone would not show what the API receives.
     */
    const leaveAlone = endpoints.updateCard("c", { title: "Renamed" }).body;
    const unassign = endpoints.updateCard("c", { assigneeId: null }).body;
    const assign = endpoints.updateCard("c", { assigneeId: "user-1" }).body;

    expect(JSON.parse(JSON.stringify(leaveAlone))).toEqual({ title: "Renamed" });
    expect(JSON.parse(JSON.stringify(unassign))).toEqual({ assignee_id: null });
    expect(JSON.parse(JSON.stringify(assign))).toEqual({ assignee_id: "user-1" });
  });

  it("clears a due date with a null rather than by omitting it", () => {
    expect(JSON.parse(JSON.stringify(endpoints.updateCard("c", { dueAt: null }).body)))
      .toEqual({ due_at: null });

    expect(
      JSON.parse(
        JSON.stringify(
          endpoints.updateCard("c", { dueAt: "2026-08-31T17:00:00.000Z" }).body,
        ),
      ),
    ).toEqual({ due_at: "2026-08-31T17:00:00.000Z" });
  });

  it("omits the create-time card fields nobody set", () => {
    // On a create there is no third state: `createCardRequest` takes plain
    // pointers, so absent and null both mean "nobody" and "no date".
    expect(
      JSON.parse(JSON.stringify(endpoints.createCard("col", { title: "New" }).body)),
    ).toEqual({ title: "New" });
  });

  it("escapes path segments", () => {
    expect(endpoints.getCard("../boards/other").path).toBe(
      "/cards/..%2Fboards%2Fother",
    );
  });

  it("omits the member role rather than sending an empty string", () => {
    // `validateAddMember` reads "" as "grant member" and anything unrecognised
    // as a 400, so an omitted key and an empty string happen to agree today.
    // Omitting is still the right shape: it says "no opinion" rather than
    // asserting a value, and it is what the Go struct's zero value means.
    expect(JSON.parse(JSON.stringify(endpoints.addMember({ email: "a@b.c" }).body)))
      .toEqual({ email: "a@b.c" });

    expect(endpoints.addMember({ email: "a@b.c", role: "admin" }).body).toEqual({
      email: "a@b.c",
      role: "admin",
    });
  });

  it("posts a member to the same /members path the list reads", () => {
    // One path, two methods — and `members` is already on the proxy allowlist
    // because of the list, so adding a member needed no widening of it.
    expect(endpoints.addMember({ email: "a@b.c" }).path).toBe("/members");
    expect(endpoints.addMember({ email: "a@b.c" }).method).toBe("POST");
    expect(endpoints.listMembers().path).toBe("/members");
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

  it("parses the narrower body POST /members answers with", () => {
    // `addedMemberBody` deliberately carries no display_name — a 201 must not
    // return anything read out of the global directory (ADR 0008). The parser
    // must therefore not require one, or every successful addition would come
    // back as `malformed`.
    expect(
      parseAddedMember({
        membership_id: "m1",
        user_id: "u1",
        email: "colleague@example.com",
        role: "member",
        joined_at: "2026-08-09T10:00:00Z",
      }),
    ).toEqual({
      membershipId: "m1",
      userId: "u1",
      email: "colleague@example.com",
      role: "member",
      joinedAt: "2026-08-09T10:00:00Z",
    });
  });

  it("keeps a card's assignee and due date apart from having none", () => {
    expect(parseCard(CARD)).toMatchObject({ assigneeId: null, dueAt: null });

    expect(
      parseCard({ ...CARD, assignee_id: "user-1", due_at: "2026-08-31T17:00:00Z" }),
    ).toMatchObject({ assigneeId: "user-1", dueAt: "2026-08-31T17:00:00Z" });
  });

  it("rejects a card body that does not mention assignment at all", () => {
    /*
     * The strictness this issue asked for, and the reason for it. `cardBody`
     * carries `assignee_id` and `due_at` on every card *without* `omitempty`,
     * so an absent key is a body this client does not understand — a renamed
     * field on the API side, or some other service answering 200.
     *
     * Defaulting to null instead would turn that into a board where nobody is
     * assigned to anything and nothing is ever due: wrong, confident, and
     * indistinguishable from the truth.
     */
    const withoutAssignee: Record<string, unknown> = { ...CARD };
    const withoutDue: Record<string, unknown> = { ...CARD };

    delete withoutAssignee.assignee_id;
    delete withoutDue.due_at;

    expect(parseCard(withoutAssignee)).toBeNull();
    expect(parseCard(withoutDue)).toBeNull();
  });

  it("rejects a card whose assignee_id is neither a string nor null", () => {
    expect(parseCard({ ...CARD, assignee_id: 12 })).toBeNull();
    expect(parseCard({ ...CARD, due_at: false })).toBeNull();
  });

  it("rejects an added-member body missing a field", () => {
    expect(
      parseAddedMember({ membership_id: "m1", user_id: "u1", email: "a@b.c" }),
    ).toBeNull();
  });
});
