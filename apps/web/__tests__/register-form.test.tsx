/**
 * The sign-up form.
 *
 * The interesting behaviour is not the happy path — it is the two states where
 * an account may exist and the user must not try to make it again: a 409, and a
 * registration that succeeded followed by a sign-in that did not.
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

const { RegisterForm } = await import("@/components/auth/register-form");

const GOOD = {
  email: "andy@example.com",
  password: "correct horse battery",
  displayName: "Andy Vorndran",
};

function fill(label: string | RegExp, value: string): void {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function fillValid(): void {
  fill("Email address", GOOD.email);
  fill("Password", GOOD.password);
  fill("Your name", GOOD.displayName);
}

function submit(): void {
  fireEvent.submit(screen.getByRole("button", { name: "Create account" }).closest("form")!);
}

function json(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

/** A `fetch` stand-in, typed so `mock.calls` reports what the form sent. */
type FetchStub = (input: string, init?: RequestInit) => Promise<Response>;

/** A fetch that answers by path, and refuses a request the form should not make. */
function router(handlers: Record<string, () => Response>) {
  return vi.fn<FetchStub>(async (path) => {
    const handler = handlers[path];

    if (handler === undefined) {
      throw new Error(`unexpected request to ${path}`);
    }

    return handler();
  });
}

beforeEach(() => {
  replace.mockClear();
  refresh.mockClear();
  vi.unstubAllGlobals();
});

describe("the form itself", () => {
  it("asks for the four things registration takes", () => {
    render(<RegisterForm returnTo="/app" />);

    expect(screen.getByLabelText("Email address")).toHaveAttribute("autocomplete", "email");
    expect(screen.getByLabelText("Password")).toHaveAttribute("autocomplete", "new-password");
    expect(screen.getByLabelText("Your name")).toHaveAttribute("autocomplete", "name");
    expect(screen.getByLabelText(/Workspace name/)).toHaveAttribute(
      "autocomplete",
      "organization",
    );
  });

  it("states the API's password rule and no other", () => {
    render(<RegisterForm returnTo="/app" />);

    expect(screen.getByLabelText("Password")).toHaveAccessibleDescription(
      /at least 12 characters.*no symbols, no digits, no capitals/i,
    );
  });

  it("names the workspace it will create when the field is left blank", () => {
    render(<RegisterForm returnTo="/app" />);

    fill("Your name", "Andy");

    expect(screen.getByLabelText(/Workspace name/)).toHaveAccessibleDescription(
      /Andy's workspace/,
    );
  });
});

describe("validation", () => {
  it("lists every problem in the order the fields appear", async () => {
    render(<RegisterForm returnTo="/app" />);
    submit();

    const summary = await screen.findByRole("alert");
    const text = summary.textContent ?? "";

    expect(text.indexOf("Email address")).toBeLessThan(text.indexOf("Password"));
    expect(text.indexOf("Password")).toBeLessThan(text.indexOf("Your name"));
    await waitFor(() => expect(document.activeElement).toBe(summary));
  });

  it("does not reach the network with a form it knows the API will reject", async () => {
    const fetchImpl = vi.fn();

    vi.stubGlobal("fetch", fetchImpl);
    render(<RegisterForm returnTo="/app" />);
    fillValid();
    fill("Password", "tooshort");
    submit();

    await screen.findByRole("alert");
    expect(fetchImpl).not.toHaveBeenCalled();
  });
});

describe("what it sends", () => {
  it("omits the workspace name when it is blank, so the API applies its own default", async () => {
    const fetchImpl = router({
      "/api/auth/register": () => json(201, { user_id: "u1" }),
      "/api/auth/login": () => json(200, { user_id: "u1" }),
    });

    vi.stubGlobal("fetch", fetchImpl);
    render(<RegisterForm returnTo="/app" />);
    fillValid();
    submit();

    await waitFor(() => expect(replace).toHaveBeenCalled());

    const body = JSON.parse(String(fetchImpl.mock.calls[0][1]?.body));

    expect(body).toEqual({
      email: GOOD.email,
      password: GOOD.password,
      display_name: GOOD.displayName,
    });
    expect("organization_name" in body).toBe(false);
  });

  it("trims what it does send", async () => {
    const fetchImpl = router({
      "/api/auth/register": () => json(201, { user_id: "u1" }),
      "/api/auth/login": () => json(200, { user_id: "u1" }),
    });

    vi.stubGlobal("fetch", fetchImpl);
    render(<RegisterForm returnTo="/app" />);
    fill("Email address", "  Andy@Example.COM  ");
    fill("Password", GOOD.password);
    fill("Your name", "  Andy Vorndran  ");
    fill(/Workspace name/, "  Vorndran Studio  ");
    submit();

    await waitFor(() => expect(replace).toHaveBeenCalled());

    expect(JSON.parse(String(fetchImpl.mock.calls[0][1]?.body))).toEqual({
      email: "andy@example.com",
      password: GOOD.password,
      display_name: "Andy Vorndran",
      organization_name: "Vorndran Studio",
    });
  });

  it("signs in afterwards, because registering returns no session", async () => {
    const fetchImpl = router({
      "/api/auth/register": () => json(201, { user_id: "u1" }),
      "/api/auth/login": () => json(200, { user_id: "u1" }),
    });

    vi.stubGlobal("fetch", fetchImpl);
    render(<RegisterForm returnTo="/app?tab=inbox" />);
    fillValid();
    submit();

    await waitFor(() => expect(replace).toHaveBeenCalledWith("/app?tab=inbox"));

    expect(fetchImpl.mock.calls.map((call) => call[0])).toEqual([
      "/api/auth/register",
      "/api/auth/login",
    ]);
    expect(refresh).toHaveBeenCalled();
  });
});

describe("a duplicate address", () => {
  it("says so, and offers the sign-in screen instead", async () => {
    vi.stubGlobal(
      "fetch",
      router({ "/api/auth/register": () => json(409, { error: "email is already registered" }) }),
    );
    render(<RegisterForm returnTo="/app" />);
    fillValid();
    submit();

    const summary = await screen.findByRole("alert");

    expect(summary).toHaveTextContent(/already exists/i);
    expect(screen.getByRole("link", { name: "Sign in instead" })).toHaveAttribute(
      "href",
      "/login",
    );
  });
});

describe("when the account exists but the session does not", () => {
  it("does not look like the whole thing failed", async () => {
    // The likely cause is a 429: register and login share the API's per-address
    // budget. Telling the user to try again would send them into a 409.
    vi.stubGlobal(
      "fetch",
      router({
        "/api/auth/register": () => json(201, { user_id: "u1" }),
        "/api/auth/login": () =>
          json(429, { error: "too many attempts, try again later" }, { "retry-after": "60" }),
      }),
    );
    render(<RegisterForm returnTo="/app?tab=inbox" />);
    fillValid();
    submit();

    const summary = await screen.findByRole("alert");

    expect(summary).toHaveTextContent("Your account was created");
    expect(summary).toHaveTextContent(/rather than signing up again/i);
    expect(screen.getByRole("link", { name: "sign in" })).toHaveAttribute(
      "href",
      "/login?next=%2Fapp%3Ftab%3Dinbox",
    );
    expect(replace).not.toHaveBeenCalled();
  });
});

describe("when we cannot tell whether an account was created", () => {
  it("says exactly that, and points at sign-in rather than a retry", async () => {
    // This is issue #34's other face: `Register` commits the user in one
    // transaction and the organization in a second, and a 500 does not say
    // which of them happened.
    vi.stubGlobal(
      "fetch",
      router({ "/api/auth/register": () => json(500, { error: "internal server error" }) }),
    );
    render(<RegisterForm returnTo="/app" />);
    fillValid();
    submit();

    const summary = await screen.findByRole("alert");

    expect(summary).toHaveTextContent(/could not confirm your account was created/i);
    expect(summary).toHaveTextContent(/do not sign up again/i);
    expect(screen.getByRole("link", { name: "Try signing in" })).toBeInTheDocument();
  });
});

describe("rate limiting", () => {
  it("blocks a resubmit until Retry-After has elapsed", async () => {
    vi.useFakeTimers();

    try {
      vi.stubGlobal(
        "fetch",
        router({
          "/api/auth/register": () =>
            json(429, { error: "too many attempts, try again later" }, { "retry-after": "30" }),
        }),
      );
      render(<RegisterForm returnTo="/app" />);
      fillValid();
      submit();

      await vi.waitFor(() =>
        expect(screen.getByRole("alert")).toHaveTextContent(/about 30 seconds/),
      );
      expect(screen.getByRole("button", { name: "Create account" })).toBeDisabled();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });

      expect(screen.getByRole("button", { name: "Create account" })).toBeEnabled();
    } finally {
      vi.useRealTimers();
    }
  });
});
