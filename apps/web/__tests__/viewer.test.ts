/**
 * Reading the signed-in user's name out of the only endpoint that has it.
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

const { loadViewer } = await import("@/lib/session/viewer");

const member = (id: string, name: string, email: string) => ({
  membershipId: `m-${id}`,
  userId: id,
  email,
  displayName: name,
  role: "member",
  joinedAt: "2026-01-01T00:00:00Z",
});

beforeEach(() => {
  call.mockReset();
});

describe("loadViewer", () => {
  it("finds the caller in the member list", async () => {
    call.mockResolvedValue({
      ok: true,
      data: [
        member("u2", "Someone Else", "else@example.com"),
        member("u1", "Andy Vorndran", "andy@example.com"),
      ],
    });

    expect(await loadViewer("u1")).toEqual({
      displayName: "Andy Vorndran",
      email: "andy@example.com",
    });
  });

  it("asks for the organization's members and nothing else", async () => {
    call.mockResolvedValue({ ok: true, data: [] });

    await loadViewer("u1");

    expect(call).toHaveBeenCalledTimes(1);
    expect(call.mock.calls[0][0]).toMatchObject({ method: "GET", path: "/members" });
  });

  it("returns null when the caller is not in the list", async () => {
    // What issue #34's half-registered account looks like from in here: a
    // session that authenticates, in an organization it is not a member of.
    call.mockResolvedValue({ ok: true, data: [member("u2", "Someone Else", "e@example.com")] });

    expect(await loadViewer("u1")).toBeNull();
  });

  it("returns null rather than throwing when the API refuses", async () => {
    for (const kind of ["unauthorized", "forbidden", "server_error", "network"]) {
      call.mockResolvedValue({ ok: false, error: { kind, message: "no", status: 500 } });

      expect(await loadViewer("u1")).toBeNull();
    }
  });
});
