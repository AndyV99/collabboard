/**
 * The wire contract of the tenantless-account recovery call.
 *
 * `POST /api/v1/organizations` is the one endpoint in this app that sends a
 * password to an *unauthenticated* route, so the two things worth pinning are
 * what goes on the wire and what does not: no `Authorization` header, because
 * there is no token an account in this state could hold (ADR 0009), and no
 * `organization_name` key at all when the user left the box blank, because an
 * empty string and an absent field reach the same `workspaceName` default on the
 * API and sending a key we do not mean is how a default gets depended on.
 *
 * The statuses matter as much as the success: 409 and 429 are both normal
 * answers here, and both drive a different screen.
 */

import { afterEach, describe, expect, it, vi } from "vitest";

import { createFirstOrganization } from "@/lib/session/auth-api";
import { parseCreatedOrganization } from "@/lib/api/types";

const BASE = "http://localhost:8080/api/v1";

const ORGANIZATION = {
  id: "9f1c6c2e-0a2e-4d5f-9a2e-0a2e4d5f9a2e",
  name: "Acme",
  slug: "acme",
  role: "owner",
};

function respond(status: number, body: unknown, headers: Record<string, string> = {}) {
  const fetchImpl = vi.fn(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json", ...headers },
      }),
  );

  vi.stubGlobal("fetch", fetchImpl);

  return fetchImpl;
}

/** The request the last call put on the wire. */
function sent(fetchImpl: ReturnType<typeof respond>) {
  const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];

  return {
    url,
    init,
    headers: init.headers as Record<string, string>,
    body: JSON.parse(String(init.body)) as Record<string, unknown>,
  };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("createFirstOrganization", () => {
  it("posts the credentials to the unauthenticated organizations route", async () => {
    const fetchImpl = respond(201, { user_id: "u1", organization: ORGANIZATION });

    await createFirstOrganization({
      email: "orphan@example.com",
      password: "correct horse battery",
    });

    const request = sent(fetchImpl);

    expect(request.url).toBe(`${BASE}/organizations`);
    expect(request.init.method).toBe("POST");
    expect(request.body.email).toBe("orphan@example.com");
    expect(request.body.password).toBe("correct horse battery");
  });

  it("sends no Authorization header, because there is no token to send", async () => {
    // Not an omission. `Login` refuses to issue a token to a subject with zero
    // memberships, `Issuer.Issue` refuses a nil tenant and `Issuer.Verify`
    // refuses a zero `org` claim — so a bearer token is not merely absent here,
    // it is unrepresentable. ADR 0009 records why widening those checks was
    // rejected.
    const fetchImpl = respond(201, { user_id: "u1", organization: ORGANIZATION });

    await createFirstOrganization({ email: "orphan@example.com", password: "pw" });

    expect(sent(fetchImpl).headers).not.toHaveProperty("authorization");
  });

  it("omits organization_name entirely when none was chosen", async () => {
    const fetchImpl = respond(201, { user_id: "u1", organization: ORGANIZATION });

    await createFirstOrganization({ email: "orphan@example.com", password: "pw" });

    expect(sent(fetchImpl).body).not.toHaveProperty("organization_name");
  });

  it("sends organization_name when one was chosen", async () => {
    const fetchImpl = respond(201, { user_id: "u1", organization: ORGANIZATION });

    await createFirstOrganization({
      email: "orphan@example.com",
      password: "pw",
      organizationName: "Acme",
    });

    expect(sent(fetchImpl).body.organization_name).toBe("Acme");
  });

  it("returns the created organization, not a session", async () => {
    // The endpoint answers 201 with no tokens on purpose: token issuance did
    // not spread to this route, and the client's next call is an ordinary login.
    respond(201, { user_id: "u1", organization: ORGANIZATION });

    const result = await createFirstOrganization({ email: "a@b.c", password: "pw" });

    expect(result).toEqual({
      ok: true,
      data: { userId: "u1", organization: { id: ORGANIZATION.id, name: "Acme", slug: "acme", role: "owner" } },
    });
  });

  it("reports a 409 as a conflict, which the screen reads as 'already resolved'", async () => {
    respond(409, { error: "this account already belongs to an organization" });

    const result = await createFirstOrganization({ email: "a@b.c", password: "pw" });

    expect(result.ok).toBe(false);
    expect(result.ok === false && result.error.kind).toBe("conflict");
  });

  it("carries Retry-After off a 429, because this shares the login budget", async () => {
    respond(429, { error: "too many attempts, try again later" }, { "retry-after": "900" });

    const result = await createFirstOrganization({ email: "a@b.c", password: "pw" });

    expect(result.ok === false && result.error.kind).toBe("rate_limited");
    expect(result.ok === false && result.error.retryAfterSeconds).toBe(900);
  });

  it("treats a 201 whose body is the wrong shape as malformed, not as success", async () => {
    respond(201, { user_id: "u1" });

    const result = await createFirstOrganization({ email: "a@b.c", password: "pw" });

    expect(result.ok === false && result.error.kind).toBe("malformed");
  });
});

describe("parseCreatedOrganization", () => {
  it("accepts the documented body", () => {
    expect(parseCreatedOrganization({ user_id: "u1", organization: ORGANIZATION })).toEqual({
      userId: "u1",
      organization: { id: ORGANIZATION.id, name: "Acme", slug: "acme", role: "owner" },
    });
  });

  it("rejects anything missing either half", () => {
    expect(parseCreatedOrganization({ organization: ORGANIZATION })).toBeNull();
    expect(parseCreatedOrganization({ user_id: "u1" })).toBeNull();
    expect(parseCreatedOrganization({ user_id: 1, organization: ORGANIZATION })).toBeNull();
    expect(parseCreatedOrganization(null)).toBeNull();
    expect(parseCreatedOrganization([])).toBeNull();
  });
});
