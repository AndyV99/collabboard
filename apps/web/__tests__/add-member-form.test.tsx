/**
 * Adding a member: what the form offers, and what it says when the API refuses.
 *
 * The role choice is the part worth guarding. ADR 0008 bounds what each role may
 * grant, and a form offering a choice the server will reject is a 400 the user
 * cannot understand — so the options come from `grantableRoles` and the page
 * decides whether to render the form at all.
 */

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const refresh = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh }),
}));

const { __resetBrowserApiForTests } = await import("@/lib/api/browser");
const { AddMemberForm } = await import("@/components/members/add-member-form");
const { ROLE_ADMIN, grantableRoles } = await import("@/lib/workspace/roles");
const { MAX_EMAIL_BYTES, maxLengthForBytes } = await import("@/lib/workspace/rules");

type FetchStub = (input: string, init?: RequestInit) => Promise<Response>;

function respond(status: number, body: unknown) {
  return vi.fn<FetchStub>(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json" },
      }),
  );
}

const ADDED = {
  member: {
    membership_id: "m-9",
    user_id: "u-9",
    email: "colleague@example.com",
    role: "member",
    joined_at: "2026-08-09T10:00:00Z",
  },
};

function fill(label: string | RegExp, value: string): void {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function submit(): void {
  fireEvent.submit(
    screen.getByRole("button", { name: "Add to workspace" }).closest("form")!,
  );
}

beforeEach(() => {
  refresh.mockClear();
  vi.unstubAllGlobals();
  __resetBrowserApiForTests();
});

describe("the choices it offers", () => {
  it("lets an owner grant member or admin, and never owner", () => {
    render(<AddMemberForm roles={grantableRoles("owner")} />);

    const options = screen.getAllByRole("option").map((option) => option.textContent);

    expect(options).toHaveLength(2);
    expect(options[0]).toContain("Member");
    expect(options[1]).toContain("Admin");
    expect(options.join(" ")).not.toContain("Owner");
  });

  it("does not show an admin a choice of one", () => {
    // A select with a single option is a control that cannot be operated. The
    // form states the role in a sentence instead.
    render(<AddMemberForm roles={grantableRoles(ROLE_ADMIN)} />);

    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.getByText(/Only an owner can add an admin/)).toBeInTheDocument();
  });

  it("says the person must already have an account", () => {
    // ADR 0008: this path never calls identity_create_user, so an unregistered
    // address is a 404 and no amount of retrying changes that.
    render(<AddMemberForm roles={grantableRoles("owner")} />);

    expect(screen.getByText(/need a CollabBoard account already/)).toBeInTheDocument();
  });
});

describe("what it sends", () => {
  it("posts the normalised address and the chosen role", async () => {
    const fetchStub = respond(201, ADDED);

    vi.stubGlobal("fetch", fetchStub);
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "  Colleague@Example.COM ");
    fireEvent.change(screen.getByRole("combobox"), { target: { value: ROLE_ADMIN } });
    submit();

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(fetchStub.mock.calls[0][0]).toBe("/api/proxy/members");
    expect(JSON.parse(String(fetchStub.mock.calls[0][1]?.body))).toEqual({
      email: "colleague@example.com",
      role: ROLE_ADMIN,
    });
  });

  it("does not send an address with nothing before the @", () => {
    // users.email carries CHECK (position('@' IN email) > 1), so such an
    // account cannot exist and the request could only ever be a 404.
    const fetchStub = respond(201, ADDED);

    vi.stubGlobal("fetch", fetchStub);
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "@example.com");
    submit();

    expect(fetchStub).not.toHaveBeenCalled();
    expect(screen.getByText(/part before the @/)).toBeInTheDocument();
  });
});

describe("what it says afterwards", () => {
  it("confirms, clears the field, and re-renders the list from the server", async () => {
    vi.stubGlobal("fetch", respond(201, ADDED));
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "colleague@example.com");
    submit();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent("Added to the workspace"),
    );
    expect(screen.getByLabelText("Email address")).toHaveValue("");
    expect(refresh).toHaveBeenCalled();
  });

  it("says nobody was notified, because nobody was", async () => {
    // There is no mailer. A confirmation implying one is how a colleague never
    // finds out they were added.
    vi.stubGlobal("fetch", respond(201, ADDED));
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "colleague@example.com");
    submit();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/were not sent anything/),
    );
  });

  it("explains a 404 as 'they have not signed up', not as a typo", async () => {
    vi.stubGlobal("fetch", respond(404, { error: "no account with that email address" }));
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "stranger@example.com");
    submit();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/needs? a CollabBoard account/i),
    );
  });

  it("points a 409 at the list already on the page", async () => {
    vi.stubGlobal("fetch", respond(409, { error: "already a member of this organization" }));
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "colleague@example.com");
    submit();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/already in this workspace/),
    );
  });

  it("treats a 403 as a role that changed underneath the page", async () => {
    // The page only renders this form for an owner or an admin, so a 403 means
    // the membership row changed since the render — not that the user should
    // try again.
    vi.stubGlobal("fetch", respond(403, { error: "your role does not permit adding a member" }));
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "colleague@example.com");
    submit();

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent(/reload/i));
    expect(refresh).not.toHaveBeenCalled();
  });

  it("moves focus to the outcome, success or failure", async () => {
    vi.stubGlobal("fetch", respond(201, ADDED));
    render(<AddMemberForm roles={grantableRoles("owner")} />);
    fill("Email address", "colleague@example.com");
    submit();

    await waitFor(() => expect(screen.getByRole("alert")).toHaveFocus());
  });
});

describe("the address field's limit", () => {
  it("is derived from the byte limit, which is a different unit again", () => {
    /*
     * `maxEmailLength` counts bytes; `maxLength` counts UTF-16 code units. The
     * two happen to coincide at the same number, because the cheapest a code
     * unit can be in UTF-8 is one byte — so the attribute can only ever be
     * looser than the API, never tighter.
     *
     * The coincidence is why this is worth an assertion. `maxLength={254}`
     * derived from a byte limit looks exactly like the #87 bug, and the next
     * reader "fixing" it to `maxLengthFor(254)` would be changing a number that
     * was already right.
     */
    render(<AddMemberForm roles={grantableRoles("owner")} />);

    expect(screen.getByLabelText("Email address")).toHaveAttribute(
      "maxlength",
      String(maxLengthForBytes(MAX_EMAIL_BYTES)),
    );
  });
});
