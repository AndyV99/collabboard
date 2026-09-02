/**
 * Reading the signed-in principal, and what happens when it cannot be read.
 *
 * This file replaces `__tests__/viewer.test.ts`, which tested the workaround
 * #78 deleted: before `GET /me` reported `email` and `display_name`, the shell
 * listed every member of the organization and searched the result for itself.
 * The assertion that endpoint is `/me` and not `/members` is therefore the
 * point of the file rather than an incidental detail — it is the whole of the
 * issue, stated as a test.
 *
 * Every failure degrades to null rather than redirecting. A redirect from here
 * would be a loop: `serverApi` cannot clear a cookie, so the sign-in page would
 * see a session, bounce the visitor straight back, and the shell would fail the
 * same way again.
 */

import { beforeEach, describe, expect, it, vi } from "vitest";

const call = vi.fn();

vi.mock("@/lib/api/server", () => ({
  serverApi: (...args: unknown[]) => call(...args),
}));

const { loadCurrentUser } = await import("@/lib/session/me");

const ORGANIZATION = { id: "o1", name: "Acme", slug: "acme", role: "owner" };

const ME = {
  userId: "u1",
  email: "andy@example.com",
  displayName: "Andy Vorndran",
  role: "owner",
  sessionId: "s1",
  organization: ORGANIZATION,
  organizations: [ORGANIZATION],
};

beforeEach(() => {
  call.mockReset();
});

describe("loadCurrentUser", () => {
  it("returns the principal the API reported", async () => {
    call.mockResolvedValue({ ok: true, data: ME });

    expect(await loadCurrentUser()).toEqual(ME);
  });

  it("asks /me, once, and never /members", async () => {
    /*
     * The acceptance criterion of #78, as an assertion. `GET /members` on a
     * protected page render was O(members) work and a read of every colleague's
     * address in order to display one name — and a `/members` in the API's
     * request log for a page that shows no members is a question nobody could
     * answer from the URL.
     */
    call.mockResolvedValue({ ok: true, data: ME });

    await loadCurrentUser();

    expect(call).toHaveBeenCalledTimes(1);
    expect(call.mock.calls[0][0]).toMatchObject({ method: "GET", path: "/me" });
  });

  it("takes no user id, so it cannot be asked about somebody else", () => {
    // The old signature needed one because it was searching a list. Asserting
    // the arity is a cheap way to keep the search from growing back.
    expect(loadCurrentUser.length).toBe(0);
  });

  it("returns null rather than throwing when the API refuses", async () => {
    for (const kind of ["unauthorized", "forbidden", "server_error", "network"]) {
      call.mockResolvedValue({ ok: false, error: { kind, message: "no", status: 500 } });

      expect(await loadCurrentUser()).toBeNull();
    }
  });

  it("returns null for a body it does not understand", async () => {
    // `malformed` is a real outcome here rather than a theoretical one:
    // `parseCurrentUser` requires `email` and `display_name`, so an API that
    // stopped sending them would degrade the shell to "Signed in" instead of
    // rendering `undefined` in the corner.
    call.mockResolvedValue({
      ok: false,
      error: { kind: "malformed", message: "unexpected response", status: 200 },
    });

    expect(await loadCurrentUser()).toBeNull();
  });
});
