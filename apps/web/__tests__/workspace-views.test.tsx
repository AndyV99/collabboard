/**
 * The presentational half of the workspace: lists, empty states, load errors,
 * loading placeholders, and the breadcrumb.
 *
 * All of these are pure components taking resolved props, which is the split
 * the rest of this app already uses — the page fetches, the component renders —
 * and it is what makes "every list has an empty, loading and error state" a
 * claim with tests behind it rather than an aspiration.
 */

import { render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: ReactNode }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn() }),
}));

const { ProjectList } = await import("@/components/projects/project-list");
const { BoardList } = await import("@/components/boards/board-list");
const { MemberList } = await import("@/components/members/member-list");
const { PageHeader, Breadcrumbs } = await import("@/components/workspace/page-header");
const { EmptyState, ListSkeleton, LoadError, Steps } = await import(
  "@/components/workspace/states"
);

const PROJECT = {
  id: "p-1",
  name: "Launch",
  description: "Everything for the September release",
  archivedAt: null,
  createdAt: "2026-08-01T09:00:00Z",
  updatedAt: "2026-08-01T09:00:00Z",
};

describe("ProjectList", () => {
  it("links each project to its own URL", () => {
    render(<ProjectList projects={[PROJECT]} />);

    expect(screen.getByRole("link", { name: /Launch/ })).toHaveAttribute(
      "href",
      "/app/projects/p-1",
    );
  });

  it("shows the description and the created date", () => {
    render(<ProjectList projects={[PROJECT]} />);

    expect(screen.getByText("Everything for the September release")).toBeInTheDocument();
    expect(screen.getByText("Created 1 Aug 2026")).toBeInTheDocument();
  });

  it("omits the description line when there is none", () => {
    // `projectBody.description` is a string and is "" rather than null when
    // unset, so the check has to be for empty rather than for absent.
    render(<ProjectList projects={[{ ...PROJECT, description: "" }]} />);

    expect(screen.getByRole("link", { name: /Launch/ }).textContent).not.toContain(
      "Everything",
    );
  });
});

describe("BoardList", () => {
  const board = {
    id: "b-1",
    projectId: "p-1",
    name: "Sprint 12",
    createdAt: "2026-08-02T09:00:00Z",
    updatedAt: "2026-08-02T09:00:00Z",
  };

  it("links a board through the project it belongs to", () => {
    render(<BoardList boards={[board]} projectId="p-1" />);

    expect(screen.getByRole("link", { name: /Sprint 12/ })).toHaveAttribute(
      "href",
      "/app/projects/p-1/boards/b-1",
    );
  });
});

describe("MemberList", () => {
  const members = [
    {
      membershipId: "m-1",
      userId: "u-1",
      email: "andy@example.com",
      displayName: "Andy",
      role: "owner",
      joinedAt: "2026-07-01T09:00:00Z",
    },
    {
      membershipId: "m-2",
      userId: "u-2",
      email: "sam@example.com",
      displayName: "Sam",
      role: "member",
      joinedAt: "2026-08-01T09:00:00Z",
    },
  ];

  it("shows everyone with their role", () => {
    render(<MemberList members={members} viewerUserId="u-1" />);

    expect(screen.getByText("Andy")).toBeInTheDocument();
    expect(screen.getByText("sam@example.com")).toBeInTheDocument();
    expect(screen.getByText("owner")).toBeInTheDocument();
    expect(screen.getByText("member")).toBeInTheDocument();
  });

  it("marks the signed-in member's own row and only that one", () => {
    render(<MemberList members={members} viewerUserId="u-2" />);

    const rows = screen.getAllByRole("listitem");

    expect(within(rows[1]).getByText("You")).toBeInTheDocument();
    expect(within(rows[0]).queryByText("You")).not.toBeInTheDocument();
  });
});

describe("EmptyState", () => {
  it("is a screen rather than an absence: heading, explanation, and the fix", () => {
    render(
      <EmptyState body={<p>A project holds boards.</p>} title="Start with your first project">
        <button type="button">Create project</button>
      </EmptyState>,
    );

    expect(
      screen.getByRole("heading", { name: "Start with your first project" }),
    ).toBeInTheDocument();
    expect(screen.getByText("A project holds boards.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create project" })).toBeInTheDocument();
  });

  it("numbers the steps for sighted users without repeating them to a screen reader", () => {
    render(<Steps steps={[{ title: "Create a project.", body: "One per client." }]} />);

    const marker = screen.getByText("1");

    expect(marker).toHaveAttribute("aria-hidden", "true");
    expect(screen.getByText("Create a project.")).toBeInTheDocument();
  });
});

describe("LoadError", () => {
  it("offers a retry when trying again could work", () => {
    render(
      <LoadError
        failure={{ title: "Could not load projects", message: "The server did not answer.", retryable: true }}
      />,
    );

    expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
  });

  it("does not offer a retry for a failure a retry cannot fix", () => {
    render(
      <LoadError
        failure={{ title: "Your session is no longer valid", message: "Sign out.", retryable: false }}
      />,
    );

    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("does not announce itself as an alert", () => {
    // It is part of the first paint, not a response to something the user did.
    // An alert here interrupts the reading order of a page just arrived at; the
    // heading puts it in the document outline instead.
    render(
      <LoadError failure={{ title: "Not found", message: "Gone.", retryable: false }} />,
    );

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});

describe("ListSkeleton", () => {
  it("announces once and hides the placeholder boxes", () => {
    render(<ListSkeleton label="Loading projects…" rows={3} />);

    expect(screen.getByRole("status")).toHaveTextContent("Loading projects…");
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
  });
});

describe("Breadcrumbs", () => {
  it("names the navigation landmark and marks the current page", () => {
    render(
      <Breadcrumbs
        crumbs={[
          { href: "/app", label: "Projects" },
          { href: "/app/projects/p-1", label: "Launch" },
          { label: "Sprint 12" },
        ]}
      />,
    );

    const nav = screen.getByRole("navigation", { name: "Breadcrumb" });

    expect(within(nav).getByRole("link", { name: "Projects" })).toHaveAttribute(
      "href",
      "/app",
    );
    expect(within(nav).queryByRole("link", { name: "Sprint 12" })).not.toBeInTheDocument();
    expect(within(nav).getByText("Sprint 12")).toHaveAttribute("aria-current", "page");
  });
});

describe("PageHeader", () => {
  it("puts the title in an h1 and the lede below it", () => {
    render(<PageHeader lede="One project." title="Projects" />);

    expect(screen.getByRole("heading", { level: 1, name: "Projects" })).toBeInTheDocument();
    expect(screen.getByText("One project.")).toBeInTheDocument();
  });
});
