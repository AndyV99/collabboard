/**
 * The sign-in form: what it sends, what it says, and where focus goes.
 *
 * `next/navigation` and `next/link` are mocked because neither has a router in a
 * unit test. Everything else — the validation, the fetch, the error mapping, the
 * focus management — is the real thing.
 */

import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const replace = vi.fn();
const refresh = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace, refresh }),
}));

vi.mock("next/link", () => ({
  default: ({ href, children }: { href: string; children: ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

const { LoginForm } = await import("@/components/auth/login-form");
const { MAX_ORGANIZATION_NAME_CODE_POINTS } = await import("@/lib/auth/rules");

function fill(label: string | RegExp, value: string): void {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function submit(): void {
  fireEvent.submit(screen.getByRole("button", { name: "Sign in" }).closest("form")!);
}

/** A `fetch` stand-in, typed so `mock.calls` reports what the form sent. */
type FetchStub = (input: string, init?: RequestInit) => Promise<Response>;

function respond(status: number, body: unknown, headers: Record<string, string> = {}) {
  return vi.fn<FetchStub>(async () =>
    new Response(JSON.stringify(body), {
      status,
      headers: { "content-type": "application/json", ...headers },
    }),
  );
}

beforeEach(() => {
  replace.mockClear();
  refresh.mockClear();
  vi.unstubAllGlobals();
});

describe("the form itself", () => {
  it("labels every field and asks the browser to autofill it", () => {
    render(<LoginForm returnTo="/app" />);

    const email = screen.getByLabelText("Email address");
    const password = screen.getByLabelText("Password");

    expect(email).toHaveAttribute("type", "email");
    expect(email).toHaveAttribute("autocomplete", "email");
    expect(password).toHaveAttribute("type", "password");
    expect(password).toHaveAttribute("autocomplete", "current-password");
  });

  it("turns off the browser's own validation, which is stricter than the API's", () => {
    render(<LoginForm returnTo="/app" />);

    expect(screen.getByRole("button", { name: "Sign in" }).closest("form")).toHaveAttribute(
      "novalidate",
    );
  });

  it("can reveal the password without changing which control it is", () => {
    render(<LoginForm returnTo="/app" />);

    const password = screen.getByLabelText("Password");

    fireEvent.click(screen.getByRole("button", { name: "Show password" }));
    expect(password).toHaveAttribute("type", "text");

    fireEvent.click(screen.getByRole("button", { name: "Hide password" }));
    expect(password).toHaveAttribute("type", "password");
  });

  it("offers sign-up before any attempt is made", () => {
    // Unconditional on purpose: a link that appeared only after an unknown
    // address would disclose that the address is unknown.
    render(<LoginForm returnTo="/app" />);

    expect(screen.getByRole("link", { name: "Create an account" })).toHaveAttribute(
      "href",
      "/register",
    );
  });

  it("carries the return destination into the sign-up link", () => {
    render(<LoginForm returnTo="/app?tab=inbox" />);

    expect(screen.getByRole("link", { name: "Create an account" })).toHaveAttribute(
      "href",
      "/register?next=%2Fapp%3Ftab%3Dinbox",
    );
  });
});

describe("an empty form", () => {
  it("does not reach the network, so it costs no rate-limit slot", async () => {
    const fetchImpl = respond(200, {});

    vi.stubGlobal("fetch", fetchImpl);
    render(<LoginForm returnTo="/app" />);
    submit();

    await screen.findByRole("alert");
    expect(fetchImpl).not.toHaveBeenCalled();
  });

  it("lists both problems and moves focus to the list", async () => {
    render(<LoginForm returnTo="/app" />);
    submit();

    const summary = await screen.findByRole("alert");

    expect(summary).toHaveTextContent("Email address: Enter your email address.");
    expect(summary).toHaveTextContent("Password: Enter your password.");
    await waitFor(() => expect(document.activeElement).toBe(summary));
  });

  it("marks the fields invalid and describes them", async () => {
    render(<LoginForm returnTo="/app" />);
    submit();
    await screen.findByRole("alert");

    const email = screen.getByLabelText("Email address");

    expect(email).toHaveAttribute("aria-invalid", "true");
    expect(email).toHaveAccessibleDescription("Enter your email address.");
  });
});

describe("what it sends", () => {
  it("normalises the address the way the API's rate limiter does", async () => {
    const fetchImpl = respond(200, { user_id: "u1" });

    vi.stubGlobal("fetch", fetchImpl);
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "  Andy@Example.COM ");
    fill("Password", "correct horse battery");
    submit();

    await waitFor(() => expect(fetchImpl).toHaveBeenCalled());

    const [path, init] = fetchImpl.mock.calls[0];

    expect(path).toBe("/api/auth/login");
    expect(init?.credentials).toBe("same-origin");
    expect(JSON.parse(String(init?.body))).toEqual({
      email: "andy@example.com",
      password: "correct horse battery",
    });
  });

  it("sends a password the API would accept but a stricter client would not", async () => {
    // Four characters. The sign-in form must not apply the sign-up length rule.
    const fetchImpl = respond(401, { error: "invalid email or password" });

    vi.stubGlobal("fetch", fetchImpl);
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "andy@example.com");
    fill("Password", "shrt");
    submit();

    await waitFor(() => expect(fetchImpl).toHaveBeenCalled());
  });
});

describe("on success", () => {
  it("navigates to the intended page and drops the cached render", async () => {
    vi.stubGlobal("fetch", respond(200, { user_id: "u1" }));
    render(<LoginForm returnTo="/app?tab=inbox" />);
    fill("Email address", "andy@example.com");
    fill("Password", "correct horse battery");
    submit();

    await waitFor(() => expect(replace).toHaveBeenCalledWith("/app?tab=inbox"));
    expect(refresh).toHaveBeenCalled();
  });

  it("keeps the button disabled through the navigation", async () => {
    vi.stubGlobal("fetch", respond(200, { user_id: "u1" }));
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "andy@example.com");
    fill("Password", "correct horse battery");
    submit();

    await waitFor(() => expect(replace).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: "Signing in…" })).toBeDisabled();
  });
});

describe("on failure", () => {
  it("says the same thing whether the address exists or not", async () => {
    // Both are the same 401 with the same body, because that is all the API
    // gives back. This asserts the form adds no second signal of its own.
    vi.stubGlobal("fetch", respond(401, { error: "invalid email or password" }));

    const { unmount } = render(<LoginForm returnTo="/app" />);

    fill("Email address", "known@example.com");
    fill("Password", "wrong password here");
    submit();

    const first = (await screen.findByRole("alert")).textContent;

    unmount();
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "unknown@example.com");
    fill("Password", "wrong password here");
    submit();

    expect((await screen.findByRole("alert")).textContent).toBe(first);
    expect(first).not.toMatch(/no account|not found|unknown|does not exist/i);
  });

  it("moves focus to the alert", async () => {
    vi.stubGlobal("fetch", respond(401, { error: "invalid email or password" }));
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "andy@example.com");
    fill("Password", "wrong password here");
    submit();

    const summary = await screen.findByRole("alert");

    await waitFor(() => expect(document.activeElement).toBe(summary));
    expect(replace).not.toHaveBeenCalled();
  });

  it("explains the half-registered account rather than saying the password is wrong", async () => {
    vi.stubGlobal(
      "fetch",
      respond(403, { error: "this account does not belong to an organization" }),
    );
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "orphan@example.com");
    fill("Password", "correct horse battery");
    submit();

    const summary = await screen.findByRole("alert");

    expect(summary).toHaveTextContent(/not attached to a workspace/i);
    expect(summary).not.toHaveTextContent(/password is incorrect/i);

    // #34 replaced the support ticket with a self-service path, so the screen
    // offers the fix rather than an address to write to.
    expect(summary).not.toHaveTextContent(/contact support/i);
    expect(
      screen.getByRole("button", { name: "Create workspace and sign in" }),
    ).toBeEnabled();
  });

  it("does not read our own CSRF refusal as a missing workspace", async () => {
    vi.stubGlobal(
      "fetch",
      respond(403, { error: "This request did not come from this site." }, {
        "x-collabboard-refusal": "cross-origin",
      }),
    );
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "andy@example.com");
    fill("Password", "correct horse battery");
    submit();

    expect(await screen.findByRole("alert")).toHaveTextContent(/did not look like it came/i);
  });

  it("reports an unreachable server as one", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new TypeError("failed to fetch"); }));
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "andy@example.com");
    fill("Password", "correct horse battery");
    submit();

    expect(await screen.findByRole("alert")).toHaveTextContent(/could not reach the server/i);
  });
});

/**
 * A `fetch` stand-in that answers per path, and records what it was sent.
 *
 * The recovery flow makes two requests to two different Route Handlers, so a
 * single-response stub cannot express it. `calls` is what the assertions about
 * "the user did not retype anything" are actually made against: the second
 * request has to carry the credentials from the first without the test ever
 * filling a field again.
 */
type Reply = { status: number; body: unknown; headers?: Record<string, string> };

function routes(replies: Record<string, Reply>) {
  const calls: { path: string; body: Record<string, unknown> }[] = [];

  const fetchImpl = vi.fn<FetchStub>(async (input, init) => {
    calls.push({ path: input, body: JSON.parse(String(init?.body)) });

    const reply = replies[input];

    if (reply === undefined) {
      throw new Error(`the form posted to an unstubbed path: ${input}`);
    }

    return new Response(JSON.stringify(reply.body), {
      status: reply.status,
      headers: { "content-type": "application/json", ...reply.headers },
    });
  });

  return { fetchImpl, calls };
}

const CREATED = {
  status: 201,
  body: { user_id: "u1", organization: { id: "o1", name: "Acme", slug: "acme", role: "owner" } },
};

const STRANDED = {
  status: 403,
  body: { error: "this account does not belong to an organization" },
};

/**
 * Gets to the state the recovery affordance appears in: a bare 403 on sign-in.
 *
 * Login stays 403 throughout, so the follow-up sign-in after a successful
 * creation also fails. That keeps the component mounted and assertable; the
 * test that cares about the happy path stubs login to change its answer.
 */
async function reachStrandedAccount(recoveryReply: Reply) {
  const { fetchImpl, calls } = routes({
    "/api/auth/login": STRANDED,
    "/api/auth/first-organization": recoveryReply,
  });

  vi.stubGlobal("fetch", fetchImpl);
  render(<LoginForm returnTo="/app" />);
  fill("Email address", "orphan@example.com");
  fill("Password", "correct horse battery");
  submit();

  await screen.findByRole("button", { name: "Create workspace and sign in" });

  return { fetchImpl, calls };
}

/**
 * Waits for the recovery request and returns it.
 *
 * Keyed on the path rather than on a call count, because a successful creation
 * is followed by a sign-in attempt — so the total is 2 or 3 depending on the
 * stub, and asserting a length is a race between the two.
 */
async function recoveryRequest(calls: { path: string; body: Record<string, unknown> }[]) {
  await waitFor(() =>
    expect(calls.some((call) => call.path === "/api/auth/first-organization")).toBe(true),
  );

  return calls.find((call) => call.path === "/api/auth/first-organization")!;
}

function recover(): void {
  fireEvent.submit(
    screen
      .getByRole("button", { name: "Create workspace and sign in" })
      .closest("form")!,
  );
}

describe("recovering an account that has no workspace", () => {
  it("creates the workspace with what was already typed, and asks for nothing again", async () => {
    const { calls } = await reachStrandedAccount(CREATED);

    // Note what the test does *not* do between the failed sign-in and this
    // click: no `fill`. The credentials on the wire below can only have come
    // from what the user typed into the sign-in form.
    recover();

    const created = await recoveryRequest(calls);

    expect(created.body.email).toBe("orphan@example.com");
    expect(created.body.password).toBe("correct horse battery");
    // Blank name omitted, so the API applies its own default rather than this
    // app duplicating it.
    expect(created.body).not.toHaveProperty("organization_name");
  });

  it("never asks for the password a second time", async () => {
    await reachStrandedAccount(CREATED);

    // One password box on the screen, the one that already has the password in
    // it. A recovery screen that re-prompted would have two. Queried off the
    // DOM rather than by label, because "Show password" on the reveal toggle
    // matches a label query for /password/ too.
    expect(document.querySelectorAll("input[type='password']")).toHaveLength(1);
    expect(screen.getByLabelText("Password")).toHaveValue("correct horse battery");
  });

  it("signs the user in and navigates once the workspace exists", async () => {
    // Login answers 403 first and 200 second: the same endpoint, a different
    // answer once the account has somewhere to be. That is the whole point of
    // the flow, so the stub models it rather than pretending login is static.
    let loginCalls = 0;

    const fetchImpl = vi.fn<FetchStub>(async (input) => {
      if (input === "/api/auth/first-organization") {
        return new Response(JSON.stringify(CREATED.body), {
          status: 201,
          headers: { "content-type": "application/json" },
        });
      }

      loginCalls += 1;

      return loginCalls === 1
        ? new Response(JSON.stringify(STRANDED.body), {
            status: 403,
            headers: { "content-type": "application/json" },
          })
        : new Response(JSON.stringify({ user_id: "u1" }), {
            status: 200,
            headers: { "content-type": "application/json" },
          });
    });

    vi.stubGlobal("fetch", fetchImpl);
    render(<LoginForm returnTo="/app?tab=inbox" />);
    fill("Email address", "orphan@example.com");
    fill("Password", "correct horse battery");
    submit();

    await screen.findByRole("button", { name: "Create workspace and sign in" });
    recover();

    await waitFor(() => expect(replace).toHaveBeenCalledWith("/app?tab=inbox"));
    expect(refresh).toHaveBeenCalled();
    expect(loginCalls).toBe(2);
  });

  it("sends a workspace name when one is given", async () => {
    const { calls } = await reachStrandedAccount(CREATED);

    fill(/workspace name/i, "  Acme Ltd  ");
    recover();

    expect((await recoveryRequest(calls)).body.organization_name).toBe("Acme Ltd");
  });

  it("keeps every field the user filled when the creation fails", async () => {
    // The explicit acceptance criterion, and the most annoying way to get this
    // wrong: a failure that empties the form makes the user retype a password
    // to try again.
    await reachStrandedAccount({
      status: 500,
      body: { error: "internal server error" },
    });

    fill(/workspace name/i, "Acme Ltd");
    recover();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/could not reach the server/i),
    );

    expect(screen.getByLabelText("Email address")).toHaveValue("orphan@example.com");
    expect(screen.getByLabelText("Password")).toHaveValue("correct horse battery");
    expect(screen.getByLabelText(/workspace name/i)).toHaveValue("Acme Ltd");
    // And the button is still there to press again.
    expect(screen.getByRole("button", { name: "Create workspace and sign in" })).toBeEnabled();
  });

  it("reports a 429 as a wait rather than as a generic failure", async () => {
    // This route is charged against the sign-in budget, before the credential
    // is even checked, so a user who just failed a few sign-ins can be refused
    // on their first click here. "Something went wrong" would be the wrong
    // answer when the real one is "wait 15 minutes".
    await reachStrandedAccount({
      status: 429,
      body: { error: "too many attempts, try again later" },
      headers: { "retry-after": "900" },
    });

    recover();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/about 15 minutes/),
    );
    expect(screen.getByRole("alert")).not.toHaveTextContent(/something went wrong/i);

    // Both buttons spend the same budget, so both stop. Retrying either one
    // early only lengthens the block.
    expect(screen.getByRole("button", { name: "Create workspace and sign in" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Sign in" })).toBeDisabled();
  });

  it("treats a 409 as resolved and points at signing in, not as an error", async () => {
    await reachStrandedAccount({
      status: 409,
      body: { error: "this account already belongs to an organization" },
    });

    recover();

    // Re-queried rather than captured, because resolving swaps the alert for a
    // different element and a captured node would be detached by now.
    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/already has a workspace/i),
    );
    expect(screen.getByRole("alert")).toHaveTextContent(/sign in below/i);

    // Nothing left to create, so the form goes away — a second attempt would
    // only collect the same 409.
    expect(
      screen.queryByRole("button", { name: "Create workspace and sign in" }),
    ).not.toBeInTheDocument();
    // The sign-in form is untouched and one press away.
    expect(screen.getByLabelText("Password")).toHaveValue("correct horse battery");
  });

  it("says the workspace was created when only the follow-up sign-in failed", async () => {
    // The dangerous state: the workspace exists but the session does not. If
    // this looked like a total failure the user would press the button again
    // and collect a 409.
    let loginCalls = 0;

    const fetchImpl = vi.fn<FetchStub>(async (input) => {
      if (input === "/api/auth/first-organization") {
        return new Response(JSON.stringify(CREATED.body), {
          status: 201,
          headers: { "content-type": "application/json" },
        });
      }

      loginCalls += 1;

      return loginCalls === 1
        ? new Response(JSON.stringify(STRANDED.body), {
            status: 403,
            headers: { "content-type": "application/json" },
          })
        : new Response(JSON.stringify({ error: "too many attempts, try again later" }), {
            status: 429,
            headers: { "content-type": "application/json", "retry-after": "60" },
          });
    });

    vi.stubGlobal("fetch", fetchImpl);
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "orphan@example.com");
    fill("Password", "correct horse battery");
    submit();

    await screen.findByRole("button", { name: "Create workspace and sign in" });
    recover();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/your workspace was created/i),
    );
    expect(screen.getByRole("alert")).toHaveTextContent(/about 1 minute/);
    expect(screen.getByRole("alert")).toHaveTextContent(/do not create another/i);
    expect(replace).not.toHaveBeenCalled();
  });

  it("refuses an over-long workspace name without reaching the network", async () => {
    // Mirrors the cap the sign-up form applies, which since #67 mirrors the
    // service: `maxOrganizationNameLength` is 200 runes and
    // `organizations.name` carries a matching CHECK. It is counted in code
    // points because the API counts runes and `String.length` counts UTF-16
    // units — 201 emoji is 402 units, and a check on `.length` would refuse a
    // name a hundred characters shorter than the limit.
    const { calls } = await reachStrandedAccount(CREATED);
    const before = calls.length;

    fill(/workspace name/i, "🔐".repeat(MAX_ORGANIZATION_NAME_CODE_POINTS + 1));
    recover();

    const alert = await screen.findByRole("alert");

    await waitFor(() =>
      expect(alert).toHaveTextContent(
        new RegExp(`at most ${MAX_ORGANIZATION_NAME_CODE_POINTS} characters`, "i"),
      ),
    );
    expect(calls).toHaveLength(before);
    expect(screen.getByLabelText(/workspace name/i)).toHaveAttribute("aria-invalid", "true");
  });

  it("clears the name error as the name is shortened", async () => {
    await reachStrandedAccount(CREATED);

    fill(/workspace name/i, "🔐".repeat(MAX_ORGANIZATION_NAME_CODE_POINTS + 1));
    recover();
    await screen.findByRole("alert");

    fill(/workspace name/i, "Acme");

    await waitFor(() =>
      expect(screen.getByLabelText(/workspace name/i)).toHaveAttribute("aria-invalid", "false"),
    );
  });

  it("does not spend a rate-limit slot when the password box was emptied", async () => {
    // Both boxes stay editable while the button is on screen, and the API
    // charges the shared sign-in budget before it checks the credential — so an
    // empty password must not become a request.
    const { calls } = await reachStrandedAccount(CREATED);
    const before = calls.length;

    fill("Password", "");
    recover();

    const alert = await screen.findByRole("alert");

    await waitFor(() => expect(alert).toHaveTextContent(/both needed/i));
    expect(calls).toHaveLength(before);
    // And the address is still there — nothing was discarded.
    expect(screen.getByLabelText("Email address")).toHaveValue("orphan@example.com");
  });

  it("does not offer recovery for a CSRF refusal, which is also a 403", async () => {
    vi.stubGlobal(
      "fetch",
      respond(403, { error: "This request did not come from this site." }, {
        "x-collabboard-refusal": "cross-origin",
      }),
    );
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "andy@example.com");
    fill("Password", "correct horse battery");
    submit();

    await screen.findByRole("alert");

    expect(
      screen.queryByRole("button", { name: "Create workspace and sign in" }),
    ).not.toBeInTheDocument();
  });

  it("leaves every other failure kind rendering exactly one plain sentence", async () => {
    vi.stubGlobal("fetch", respond(401, { error: "invalid email or password" }));
    render(<LoginForm returnTo="/app" />);
    fill("Email address", "andy@example.com");
    fill("Password", "wrong password here");
    submit();

    const alert = await screen.findByRole("alert");

    expect(alert).toHaveTextContent("Email or password is incorrect.");
    expect(screen.queryByLabelText(/workspace name/i)).not.toBeInTheDocument();
  });
});

describe("where the password does not go", () => {
  it("never leaves memory: not storage, not a cookie, not a URL", async () => {
    // The decision this feature turns on. The password is held across the
    // transition in React state — where a controlled input already held it —
    // and nowhere a later reader could find it.
    const { calls } = await reachStrandedAccount(CREATED);

    recover();
    await recoveryRequest(calls);

    const secret = "correct horse battery";

    expect(window.localStorage.length).toBe(0);
    expect(window.sessionStorage.length).toBe(0);
    expect(document.cookie).not.toContain(secret);
    expect(window.location.href).not.toContain(secret);

    // It travels as a JSON body and only as a JSON body — never a query string.
    for (const call of calls) {
      expect(call.path).not.toContain(secret);
      expect(call.path).not.toContain("password");
    }
  });
});

describe("rate limiting", () => {
  it("stops the user spending more of the budget until Retry-After has passed", async () => {
    vi.useFakeTimers();

    try {
      vi.stubGlobal(
        "fetch",
        respond(429, { error: "too many attempts, try again later" }, { "retry-after": "60" }),
      );
      render(<LoginForm returnTo="/app" />);
      fill("Email address", "andy@example.com");
      fill("Password", "correct horse battery");
      submit();

      await vi.waitFor(() =>
        expect(screen.getByRole("alert")).toHaveTextContent(/about 1 minute/),
      );

      const button = screen.getByRole("button", { name: "Sign in" });

      expect(button).toBeDisabled();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });

      expect(screen.getByRole("button", { name: "Sign in" })).toBeEnabled();
    } finally {
      vi.useRealTimers();
    }
  });
});
