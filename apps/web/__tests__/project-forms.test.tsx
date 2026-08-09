/**
 * The project forms: creating, renaming, and the one-way door.
 *
 * `next/navigation` is mocked because there is no router in a unit test.
 * Everything else — the validation, the request that goes to `/api/proxy`, the
 * error mapping, the focus management — is the real thing.
 */

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const push = vi.fn();
const refresh = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, refresh }),
}));

const { __resetBrowserApiForTests } = await import("@/lib/api/browser");
const { CreateProjectForm } = await import("@/components/projects/create-project-form");
const { RenameProjectForm } = await import("@/components/projects/rename-project-form");
const { ArchiveProject } = await import("@/components/projects/archive-project");
const { CreateBoardForm } = await import("@/components/boards/create-board-form");

/** The project as a component receives it: parsed, camelCase. */
const PROJECT = {
  id: "p-1",
  name: "Launch",
  description: "Everything for the September release",
  archivedAt: null,
  createdAt: "2026-08-01T09:00:00Z",
  updatedAt: "2026-08-01T09:00:00Z",
};

/**
 * The same project as the API sends it: snake_case, on the wire.
 *
 * Written out rather than derived from PROJECT, because the whole point of
 * `lib/api/types.ts` is that a response which is not this shape becomes a
 * `malformed` error instead of a value. A fixture that shared a shape with the
 * parsed type would test nothing about the parsing.
 */
const PROJECT_BODY = {
  id: "p-1",
  name: "Launch",
  description: "Everything for the September release",
  archived_at: null,
  created_at: "2026-08-01T09:00:00Z",
  updated_at: "2026-08-01T09:00:00Z",
};

/** A `fetch` stand-in, typed so `mock.calls` reports what a form sent. */
type FetchStub = (input: string, init?: RequestInit) => Promise<Response>;

function respond(status: number, body: unknown, headers: Record<string, string> = {}) {
  return vi.fn<FetchStub>(
    async () =>
      new Response(status === 204 ? null : JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json", ...headers },
      }),
  );
}

function fill(label: string | RegExp, value: string): void {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

function submitVia(name: string | RegExp): void {
  fireEvent.submit(screen.getByRole("button", { name }).closest("form")!);
}

/** The request body a stubbed fetch was called with. */
function sentBody(stub: ReturnType<typeof respond>): unknown {
  return JSON.parse(String(stub.mock.calls[0][1]?.body));
}

beforeEach(() => {
  push.mockClear();
  refresh.mockClear();
  vi.unstubAllGlobals();
  __resetBrowserApiForTests();
});

describe("CreateProjectForm", () => {
  it("refuses a blank name before sending anything", () => {
    const fetchStub = respond(201, {});

    vi.stubGlobal("fetch", fetchStub);
    render(<CreateProjectForm />);
    submitVia("Create project");

    expect(fetchStub).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("Name:");
  });

  it("links each problem to the field it is about", async () => {
    render(<CreateProjectForm />);
    submitVia("Create project");

    const link = screen.getByRole("link", { name: /^Name:/ });
    const target = screen.getByLabelText("Project name");

    // The generated id, not a guessed one — three forms on the project page
    // each have a field called `name`, so `#name` would be ambiguous.
    expect(link.getAttribute("href")).toBe(`#${target.id}`);
  });

  it("posts to the app's own proxy, never to the API's origin", async () => {
    const fetchStub = respond(201, { project: { ...PROJECT_BODY, id: "p-new" } });

    vi.stubGlobal("fetch", fetchStub);
    render(<CreateProjectForm />);
    fill("Project name", "Launch");
    submitVia("Create project");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(fetchStub.mock.calls[0][0]).toBe("/api/proxy/projects");
    expect(fetchStub.mock.calls[0][1]?.credentials).toBe("same-origin");
  });

  it("trims what it sends, because the API stores the trimmed value", async () => {
    const fetchStub = respond(201, { project: PROJECT_BODY });

    vi.stubGlobal("fetch", fetchStub);
    render(<CreateProjectForm />);
    fill("Project name", "  Launch  ");
    fill(/^Description/, "  Q3  ");
    submitVia("Create project");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(sentBody(fetchStub)).toEqual({ name: "Launch", description: "Q3" });
  });

  it("goes to the new project rather than back to the list", async () => {
    // A project with no boards is not a destination anyone wanted; the next
    // thing every user does is create a board.
    vi.stubGlobal("fetch", respond(201, { project: { ...PROJECT_BODY, id: "p-new" } }));
    render(<CreateProjectForm />);
    fill("Project name", "Launch");
    submitVia("Create project");

    await waitFor(() => expect(push).toHaveBeenCalledWith("/app/projects/p-new"));
    expect(refresh).toHaveBeenCalled();
  });

  it("shows the API's own sentence for a 400", async () => {
    vi.stubGlobal("fetch", respond(400, { error: "name is too long" }));
    render(<CreateProjectForm />);
    fill("Project name", "Launch");
    submitVia("Create project");

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent("name is too long"),
    );
    expect(push).not.toHaveBeenCalled();
  });

  it("moves focus to the failure so a keyboard user meets it", async () => {
    vi.stubGlobal("fetch", respond(500, { error: "internal server error" }));
    render(<CreateProjectForm />);
    fill("Project name", "Launch");
    submitVia("Create project");

    await waitFor(() => expect(screen.getByRole("alert")).toHaveFocus());
  });

  it("does not echo a 5xx body, which is a constant on this API", async () => {
    vi.stubGlobal("fetch", respond(500, { error: "internal server error" }));
    render(<CreateProjectForm />);
    fill("Project name", "Launch");
    submitVia("Create project");

    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    expect(screen.getByRole("alert")).not.toHaveTextContent("internal server error");
  });
});

describe("RenameProjectForm", () => {
  it("starts with the stored values and Save disabled", () => {
    // A PATCH mentioning neither field is a 400 — "at least one of name or
    // description is required" — which is a confusing thing to be told for
    // pressing Save on a form you did not edit.
    render(<RenameProjectForm project={PROJECT} />);

    expect(screen.getByLabelText("Project name")).toHaveValue("Launch");
    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("stays disabled when only whitespace was added", () => {
    render(<RenameProjectForm project={PROJECT} />);
    fill("Project name", "  Launch  ");

    expect(screen.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("sends both fields, so clearing the description actually clears it", async () => {
    // Omitting the key would mean "leave it alone" to the Go handler, which is
    // the opposite of what an emptied box means.
    const fetchStub = respond(200, { project: { ...PROJECT_BODY, description: "" } });

    vi.stubGlobal("fetch", fetchStub);
    render(<RenameProjectForm project={PROJECT} />);
    fill(/^Description/, "");
    submitVia("Save changes");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(fetchStub.mock.calls[0][1]?.method).toBe("PATCH");
    expect(sentBody(fetchStub)).toEqual({ name: "Launch", description: "" });
  });

  it("re-seeds from the response and disables Save again", async () => {
    vi.stubGlobal("fetch", respond(200, { project: { ...PROJECT_BODY, name: "Relaunch" } }));
    render(<RenameProjectForm project={PROJECT} />);
    fill("Project name", "Relaunch");
    submitVia("Save changes");

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Saved"));
    expect(screen.getByLabelText("Project name")).toHaveValue("Relaunch");
    expect(refresh).toHaveBeenCalled();
  });

  it("reports a 404 as gone-or-not-yours without guessing", async () => {
    vi.stubGlobal("fetch", respond(404, { error: "project not found" }));
    render(<RenameProjectForm project={PROJECT} />);
    fill("Project name", "Relaunch");
    submitVia("Save changes");

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(
        /workspace you are not a member of/,
      ),
    );
  });
});

describe("ArchiveProject", () => {
  it("states the three consequences before the confirmation", () => {
    render(<ArchiveProject project={PROJECT} />);

    const panel = screen.getByText(/Archiving cannot be undone/).closest("form")!;

    expect(panel).toHaveTextContent("no way to restore an archived project");
    expect(panel).toHaveTextContent("not deleted");
    // The one people discover the hard way: GetProject has no archived_at
    // predicate, so a saved link is the only route back to the boards.
    expect(panel).toHaveTextContent("address keeps working");
  });

  it("keeps the button disabled until the name is typed exactly", () => {
    render(<ArchiveProject project={PROJECT} />);

    const button = screen.getByRole("button", { name: "Archive permanently" });

    expect(button).toBeDisabled();

    fill("Project name", "launch");
    expect(button).toBeDisabled();

    fill("Project name", "Launch");
    expect(button).toBeEnabled();
  });

  it("tolerates whitespace around a pasted name", () => {
    render(<ArchiveProject project={PROJECT} />);
    fill("Project name", "  Launch  ");

    expect(screen.getByRole("button", { name: "Archive permanently" })).toBeEnabled();
  });

  it("sends nothing while the confirmation is unsatisfied", () => {
    const fetchStub = respond(200, { project: PROJECT_BODY });

    vi.stubGlobal("fetch", fetchStub);
    render(<ArchiveProject project={PROJECT} />);
    submitVia("Archive permanently");

    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("archives and sends the user where the consequence is visible", async () => {
    const fetchStub = respond(200, {
      project: { ...PROJECT_BODY, archived_at: "2026-08-09T10:00:00Z" },
    });

    vi.stubGlobal("fetch", fetchStub);
    render(<ArchiveProject project={PROJECT} />);
    fill("Project name", "Launch");
    submitVia("Archive permanently");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(fetchStub.mock.calls[0][0]).toBe("/api/proxy/projects/p-1/archive");
    expect(fetchStub.mock.calls[0][1]?.method).toBe("POST");

    // The project list is the screen where "it is gone" is legible; leaving the
    // user on a page describing something they can no longer find is not.
    await waitFor(() => expect(push).toHaveBeenCalledWith("/app"));
  });
});

describe("CreateBoardForm", () => {
  it("creates a board inside the project it was given", async () => {
    const fetchStub = respond(201, {
      board: {
        id: "b-new",
        project_id: "p-1",
        name: "Sprint 12",
        created_at: "2026-08-09T10:00:00Z",
        updated_at: "2026-08-09T10:00:00Z",
      },
    });

    vi.stubGlobal("fetch", fetchStub);
    render(<CreateBoardForm projectId="p-1" />);
    fill("Board name", "Sprint 12");
    submitVia("Create board");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(fetchStub.mock.calls[0][0]).toBe("/api/proxy/projects/p-1/boards");
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith("/app/projects/p-1/boards/b-new"),
    );
  });

  it("refuses a blank name without an error summary it does not need", () => {
    // One field, so a summary could only ever list one item — a second copy of
    // the field's own message and one more thing for a screen reader to read.
    const fetchStub = respond(201, {});

    vi.stubGlobal("fetch", fetchStub);
    render(<CreateBoardForm projectId="p-1" />);
    submitVia("Create board");

    expect(fetchStub).not.toHaveBeenCalled();
    expect(screen.getByText("Give the board a name.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("tells the user to reload when the project has gone", async () => {
    vi.stubGlobal("fetch", respond(404, { error: "project not found" }));
    render(<CreateBoardForm projectId="p-1" />);
    fill("Board name", "Sprint 12");
    submitVia("Create board");

    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Reload"));
  });
});
