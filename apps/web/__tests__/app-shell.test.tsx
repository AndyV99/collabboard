/**
 * The signed-in shell, rendered from resolved props.
 *
 * Pure and synchronous, which is what makes "the API could not tell us who we
 * are" a testable branch rather than something you find out in production.
 */

import { render, screen } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn(), refresh: vi.fn() }),
}));

const { AppShell } = await import("@/components/app-shell");

const ORG = { id: "o1", name: "Vorndran Studio", slug: "vorndran-studio", role: "owner" };

function shell(props: { viewer?: { displayName: string; email: string } | null; children?: ReactNode }) {
  return render(
    <AppShell organization={ORG} viewer={props.viewer ?? null}>
      {props.children ?? <p>content</p>}
    </AppShell>,
  );
}

describe("AppShell", () => {
  it("shows the organization and the role in it", () => {
    shell({ viewer: { displayName: "Andy", email: "andy@example.com" } });

    expect(screen.getByText("Vorndran Studio")).toBeInTheDocument();
    expect(screen.getByText("owner")).toBeInTheDocument();
  });

  it("shows who is signed in", () => {
    shell({ viewer: { displayName: "Andy Vorndran", email: "andy@example.com" } });

    expect(screen.getByText("Andy Vorndran")).toBeInTheDocument();
    expect(screen.getByText("andy@example.com")).toBeInTheDocument();
  });

  it("still renders the workspace when the API could not name the user", () => {
    // The workspace comes from the session cookie and costs no request, so an
    // API outage costs a name in the corner rather than the page.
    shell({ viewer: null });

    expect(screen.getByText("Vorndran Studio")).toBeInTheDocument();
    expect(screen.getByText("Signed in")).toBeInTheDocument();
  });

  it("omits the role badge when the API did not state one", () => {
    render(
      <AppShell organization={{ ...ORG, role: "" }} viewer={null}>
        <p>content</p>
      </AppShell>,
    );

    expect(screen.queryByText("owner")).not.toBeInTheDocument();
  });

  it("puts sign-out on a button, not a link", () => {
    // A GET that ends a session is one a prefetcher can perform on the user's
    // behalf, and Next prefetches links.
    shell({});

    const signOut = screen.getByRole("button", { name: "Sign out" });

    expect(signOut).toHaveAttribute("type", "button");
    expect(screen.queryByRole("link", { name: /sign out/i })).not.toBeInTheDocument();
  });

  it("renders the page inside a main landmark", () => {
    shell({ children: <h1>Your workspace</h1> });

    expect(screen.getByRole("main")).toContainElement(
      screen.getByRole("heading", { name: "Your workspace" }),
    );
  });
});
