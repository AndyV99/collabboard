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
    expect(summary).toHaveTextContent(/contact support/i);
    expect(summary).not.toHaveTextContent(/password is incorrect/i);
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
