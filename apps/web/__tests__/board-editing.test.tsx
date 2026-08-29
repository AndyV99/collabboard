/**
 * Editing the board: what is sent, what appears before the answer arrives, and
 * — the part this issue is really about — what is left on screen when the
 * server says no.
 *
 * # How the rollback tests are built so that they can fail
 *
 * A rollback test that only asserts "the card is gone afterwards" also passes
 * against a board that never showed the card in the first place. So every one
 * of them below does three things in order, against a `fetch` that does not
 * answer until the test tells it to:
 *
 *   1. submit, and assert the optimistic change **is** on screen while the
 *      request is in flight — this fails if the UI is not optimistic at all;
 *   2. settle the request with a refusal the API really produces;
 *   3. assert the change is **gone** and the failure is announced — this fails
 *      if the UI applies the change and never reverts it.
 *
 * Both halves were confirmed to fail against a deliberately broken
 * implementation before this was called done; the note in the PR says which
 * lines were broken and what each one produced.
 *
 * `next/navigation` is mocked because there is no router in a unit test.
 * Everything else — the validation, the request through `/api/proxy`, the error
 * mapping, the optimistic reducer — is the real thing.
 *
 * # Never assert optimistic DOM after an immediately-resolving `respond(...)`
 *
 * This is the one way to write a flaky test in this file, and it was written
 * once. `refresh` here is a bare `vi.fn()`, so no new props ever arrive: on
 * success the transition ends, React discards the optimistic value, and the
 * board re-renders from the *unchanged* `snapshot` prop. The optimistic state
 * therefore exists only between the click and the transition ending, and with
 * `respond(...)` that is not a state — it is a race, which loses under
 * full-suite load.
 *
 * So each assertion belongs to exactly one of three shapes:
 *
 *   - **the change is visible before the answer** → use {@link pending}, which
 *     holds the response open so the window genuinely lasts;
 *   - **the right request went out** → assert on the stub, or on `refresh` /
 *     `push` having been called; a mock call is permanent once it happens;
 *   - **the change stuck** → `respond(...)`, wait for `refresh`, then
 *     `rerender` with the board the server would now return. That is what
 *     `router.refresh()` does in production, and it is the only honest way to
 *     assert a *durable* result here.
 */

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { withRealtime } from "./support/realtime";

const push = vi.fn();
const refresh = vi.fn();

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push, refresh }),
}));

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: ReactNode }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

const { __resetBrowserApiForTests } = await import("@/lib/api/browser");
const { BoardView } = await import("@/components/boards/board-view");
const { groupCardsIntoColumns } = await import("@/lib/board/snapshot");

const BOARD = "b-1";
const PROJECT = "p-1";

/** A `fetch` stand-in, typed so `mock.calls` reports what a control sent. */
type FetchStub = (input: string, init?: RequestInit) => Promise<Response>;

function column(id: string, name: string) {
  return {
    id,
    boardId: BOARD,
    name,
    createdAt: "2026-08-01T09:00:00Z",
    updatedAt: "2026-08-01T09:00:00Z",
  };
}

function card(id: string, columnId: string, title: string, description = "") {
  return {
    id,
    boardId: BOARD,
    columnId,
    title,
    description,
    createdAt: "2026-08-02T09:00:00Z",
    updatedAt: "2026-08-02T09:00:00Z",
  };
}

/**
 * The same rows as the API sends them: snake_case, on the wire.
 *
 * Written out rather than derived from the parsed fixtures above, for the
 * reason `lib/api/types.ts` exists: a response that is not this shape becomes a
 * `malformed` error rather than a value. A stub that returned the *parsed*
 * shape would be testing nothing about the parsing, and — as this file found
 * out the hard way — would make every success look like a failure.
 */
function columnBody(id: string, name: string) {
  return {
    id,
    board_id: BOARD,
    name,
    created_at: "2026-08-01T09:00:00Z",
    updated_at: "2026-08-01T09:00:00Z",
  };
}

function cardBody(id: string, columnId: string, title: string, description = "") {
  return {
    id,
    board_id: BOARD,
    column_id: columnId,
    title,
    description,
    created_at: "2026-08-02T09:00:00Z",
    updated_at: "2026-08-02T09:00:00Z",
  };
}

const COLUMNS = [column("c-todo", "To do"), column("c-doing", "Doing"), column("c-done", "Done")];

const CARDS = [
  card("card-3", "c-doing", "Zebra"),
  card("card-2", "c-doing", "Kilo", "Some detail"),
  card("card-1", "c-doing", "Alpha"),
  card("card-4", "c-todo", "Mike"),
];

function renderBoard(
  columns: ReturnType<typeof column>[] = COLUMNS,
  cards: ReturnType<typeof card>[] = CARDS,
  selectedCardId: string | null = null,
) {
  return render(
    <BoardView
      boardId={BOARD}
      projectId={PROJECT}
      selectedCardId={selectedCardId}
      snapshot={groupCardsIntoColumns(columns as never, cards as never)}
    />,
  );
}

/**
 * A `fetch` that hangs until the test settles it.
 *
 * The in-flight window is where the optimistic state lives, and a stub that
 * answers immediately closes that window before anything can be asserted about
 * it. Holding the response open is what makes "it appeared, then it went away"
 * two observable facts instead of one guess.
 */
function pending() {
  let release!: (response: Response) => void;

  const promise = new Promise<Response>((resolve) => {
    release = resolve;
  });

  const fetchStub = vi.fn<FetchStub>(() => promise);

  return {
    fetchStub,
    settle(status: number, body: unknown = {}) {
      release(
        new Response(status === 204 ? null : JSON.stringify(body), {
          status,
          headers: { "content-type": "application/json" },
        }),
      );
    },
  };
}

/** An immediate answer, for the paths where the in-flight moment is not the point. */
function respond(status: number, body: unknown = {}) {
  return vi.fn<FetchStub>(
    async () =>
      new Response(status === 204 ? null : JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json" },
      }),
  );
}

type Stub = ReturnType<typeof respond>;

/** The JSON a stubbed fetch was called with. */
function sentBody(stub: Stub, callIndex = 0): unknown {
  return JSON.parse(String(stub.mock.calls[callIndex][1]?.body));
}

function sentTo(stub: Stub, callIndex = 0): string {
  return String(stub.mock.calls[callIndex][0]);
}

/** The HTTP method a stubbed fetch was called with. */
function sentMethod(stub: Stub, callIndex = 0): string {
  return String(stub.mock.calls[callIndex][1]?.method);
}

/** Opens a column's editing panel. */
function openColumnTools(name: string) {
  fireEvent.click(screen.getByRole("button", { name: `Edit ${name}` }));
}

/** Opens a column's card composer. */
function openCardComposer(name: string) {
  fireEvent.click(screen.getByRole("button", { name: `Add a card to ${name}` }));
}

/** The failure banner the board raises for a refused write. */
function failureBanner() {
  return screen.queryByRole("alert");
}

/**
 * A column's cards, top to bottom.
 *
 * The card's own text, not the whole list item's: since #65 each row also
 * carries a grip button whose accessible name repeats the title, so
 * `li.textContent` reads "⠿Move ZebraZebra" and an order assertion written
 * against it is really asserting the shape of a control it does not care about.
 */
function cardOrder(list: HTMLElement): string[] {
  return within(list)
    .getAllByRole("listitem")
    .map((li) => {
      const grip = within(li).queryByRole("button");

      return Array.from(li.childNodes)
        .filter((node) => node !== grip)
        .map((node) => node.textContent ?? "")
        .join("");
    });
}

/**
 * Column names, in the order the board is drawing them.
 *
 * Filtered by the heading's own id rather than taken as "every level-3
 * heading": the failure banner is also an `<h3>`, so the unfiltered version
 * silently turned every rollback assertion into a comparison against
 * `["That change was not saved", …]` — which failed for the right reason and
 * the wrong cause.
 */
function columnOrder(): string[] {
  return screen
    .getAllByRole("heading", { level: 3 })
    .filter((heading) => heading.id.startsWith("column-"))
    .map((heading) => heading.textContent ?? "");
}

beforeEach(() => {
  push.mockClear();
  refresh.mockClear();
  vi.unstubAllGlobals();
  __resetBrowserApiForTests();
});

describe("adding a card", () => {
  it("posts the title to the column's own endpoint, through the proxy", async () => {
    const fetchStub = respond(201, { card: cardBody("card-new", "c-doing", "Fresh") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "  Fresh  " },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());

    expect(sentTo(fetchStub)).toBe("/api/proxy/columns/c-doing/cards");
    // Trimmed, because the API trims before storing and the value validated
    // should be the value sent.
    expect(sentBody(fetchStub)).toEqual({ title: "Fresh" });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("shows the card at the bottom of the column before the server answers", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "Fresh" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    const doing = await screen.findByRole("list", { name: "Doing" });

    await waitFor(() =>
      expect(within(doing).getByText("Fresh")).toBeInTheDocument(),
    );

    // Appended, matching where CreateCard puts it.
    expect(cardOrder(doing)).toEqual([
      "Zebra",
      "KiloSome detail",
      "Alpha",
      "FreshAdding…",
    ]);

    settle(201, { card: cardBody("card-new", "c-doing", "Fresh") });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("does not make the unconfirmed card a link, because its id is invented", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "Fresh" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    await waitFor(() => expect(screen.getByText("Fresh")).toBeInTheDocument());

    // `?card=pending:…` would open a detail panel for a card the server has
    // never heard of.
    expect(screen.queryByRole("link", { name: /Fresh/ })).not.toBeInTheDocument();

    settle(201, { card: cardBody("card-new", "c-doing", "Fresh") });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("clears the field and keeps the composer open, so the next card is one sentence away", async () => {
    const fetchStub = respond(201, { card: cardBody("card-new", "c-doing", "Fresh") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");

    const field = screen.getByLabelText("New card in Doing");

    fireEvent.change(field, { target: { value: "Fresh" } });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    await waitFor(() => expect(field).toHaveValue(""));
    expect(screen.getByLabelText("New card in Doing")).toBeInTheDocument();
  });

  it("refuses a title over the API's limit without sending anything", () => {
    const fetchStub = respond(201, {});

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "x".repeat(201) },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    // 200 is `maxNameLength` in crud.go, counted in runes. One over is the 400
    // this saves a round trip on.
    expect(screen.getByText("Card titles can be at most 200 characters.")).toBeInTheDocument();
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("accepts a title of exactly the limit, because the API does", async () => {
    const fetchStub = respond(201, { card: cardBody("card-new", "c-doing", "x") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "x".repeat(200) },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    // `utf8.RuneCountInString(trimmed) > limit` — 200 passes, so a client that
    // rejected it would be refusing input the server accepts.
    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
  });

  it("refuses a blank title without sending anything", () => {
    const fetchStub = respond(201, {});

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "   " },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    expect(screen.getByText("Give the card a title.")).toBeInTheDocument();
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("ROLLBACK: takes the card back off the board when the server refuses it", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "Doomed" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    // 1. It is on the board while the request is in flight.
    await waitFor(() => expect(screen.getByText("Doomed")).toBeInTheDocument());

    // 2. The server refuses it. A title the client's own guard would have caught
    //    is exactly what a stale client sends, and it is a real 400.
    settle(400, { error: "title is too long" });

    // 3. It is gone, and the board says so.
    await waitFor(() => expect(screen.queryByText("Doomed")).not.toBeInTheDocument());
    expect(failureBanner()).toHaveTextContent("title is too long");
    expect(refresh).not.toHaveBeenCalled();
  });

  it("gives back what was typed when the write fails, so nothing is lost", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "Doomed" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    settle(500, { error: "internal server error" });

    await waitFor(() =>
      expect(screen.getByLabelText("New card in Doing")).toHaveValue("Doomed"),
    );
  });

  it("shows the server's own card once the refreshed board arrives", async () => {
    const fetchStub = respond(201, { card: cardBody("card-new", "c-doing", "Fresh") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));

    const view = renderBoard();

    openCardComposer("Doing");
    fireEvent.change(screen.getByLabelText("New card in Doing"), {
      target: { value: "Fresh" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    await waitFor(() => expect(refresh).toHaveBeenCalled());

    // `refresh` having been CALLED is not the transition having ENDED, and the
    // gap between those two is where this test used to lose a race (#147). It
    // failed on unrelated PRs roughly one run in three under full-suite load,
    // and never once locally.
    //
    // While the transition is still pending React keeps showing the optimistic
    // card, which is deliberately not a link because its id is invented -- the
    // case two tests above asserts exactly that. Re-rendering into that window
    // leaves `getByRole("link")` searching until its 1000ms budget runs out on
    // a loaded runner.
    //
    // The transition has ended when React has discarded the optimistic value
    // and re-rendered from the *unchanged* snapshot prop -- which has no card
    // called "Fresh" at all. So its absence is the signal, and waiting for it
    // is waiting for the thing that actually has to happen rather than for a
    // longer timeout.
    await waitFor(() => expect(screen.queryByText("Fresh")).not.toBeInTheDocument());

    // What `router.refresh()` produces in production: the Server Component runs
    // again and the page re-renders with the stored row. The optimistic value
    // is dropped and this is what is left.
    view.rerender(
      <BoardView
        boardId={BOARD}
        projectId={PROJECT}
        selectedCardId={null}
        snapshot={groupCardsIntoColumns(COLUMNS as never, [
          ...CARDS,
          card("card-new", "c-doing", "Fresh"),
        ] as never)}
      />,
    );

    // Now it is a real card: it has the server's id and is a link again.
    await waitFor(() =>
      expect(screen.getByRole("link", { name: "Fresh" })).toHaveAttribute(
        "href",
        "/app/projects/p-1/boards/b-1?card=card-new#card",
      ),
    );
  });
});

describe("adding a column", () => {
  it("posts the name to the board's columns endpoint", async () => {
    const fetchStub = respond(201, { column: columnBody("c-new", "Blocked") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    fireEvent.click(screen.getByRole("button", { name: "+ Add a column" }));
    fireEvent.change(screen.getByLabelText("New column name"), {
      target: { value: "Blocked" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add column" }).closest("form")!);

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(sentTo(fetchStub)).toBe("/api/proxy/boards/b-1/columns");
    expect(sentBody(fetchStub)).toEqual({ name: "Blocked" });
  });

  it("refuses a name over the API's limit without sending anything", () => {
    const fetchStub = respond(201, {});

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    fireEvent.click(screen.getByRole("button", { name: "+ Add a column" }));
    fireEvent.change(screen.getByLabelText("New column name"), {
      target: { value: "x".repeat(201) },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add column" }).closest("form")!);

    expect(screen.getByText("Column names can be at most 200 characters.")).toBeInTheDocument();
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("offers the form on a board with no columns at all", async () => {
    const fetchStub = respond(201, { column: columnBody("c-new", "To do") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard([], []);

    expect(screen.getByText("This board has no columns yet")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "Add the first column" }));
    fireEvent.change(screen.getByLabelText("New column name"), {
      target: { value: "To do" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add column" }).closest("form")!);

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
  });

  it("ROLLBACK: takes the column back off the board when the server refuses it", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    fireEvent.click(screen.getByRole("button", { name: "+ Add a column" }));
    fireEvent.change(screen.getByLabelText("New column name"), {
      target: { value: "Blocked" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add column" }).closest("form")!);

    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Doing", "Done", "Blocked"]));

    settle(404, { error: "board not found" });

    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Doing", "Done"]));
    expect(failureBanner()).toHaveTextContent(/no longer exists/);
  });
});

describe("renaming a column", () => {
  it("patches the column and shows the new name immediately", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("Doing");
    fireEvent.change(screen.getByLabelText("Column name"), {
      target: { value: "In progress" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Save name" }).closest("form")!);

    await waitFor(() => expect(columnOrder()).toContain("In progress"));
    expect(sentTo(fetchStub)).toBe("/api/proxy/columns/c-doing");
    expect(sentBody(fetchStub)).toEqual({ name: "In progress" });

    settle(200, { column: columnBody("c-doing", "In progress") });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("closes without a request when the name was not changed", () => {
    const fetchStub = respond(200, {});

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("Doing");
    fireEvent.submit(screen.getByRole("button", { name: "Save name" }).closest("form")!);

    // `PATCH /columns/:id` answers 400 to a body that changes nothing, and
    // "name is required" is a strange reply to someone who changed nothing.
    expect(fetchStub).not.toHaveBeenCalled();
    expect(screen.queryByLabelText("Column name")).not.toBeInTheDocument();
  });

  it("ROLLBACK: puts the old name back when the server refuses the rename", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("Doing");
    fireEvent.change(screen.getByLabelText("Column name"), {
      target: { value: "In progress" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Save name" }).closest("form")!);

    await waitFor(() => expect(columnOrder()).toEqual(["To do", "In progress", "Done"]));

    settle(404, { error: "column not found" });

    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Doing", "Done"]));
    expect(failureBanner()).toHaveTextContent(/no longer exists/);
  });
});

describe("reordering a column", () => {
  it("names the new neighbour rather than a position", async () => {
    const fetchStub = respond(200, { column: columnBody("c-todo", "To do") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("To do");
    fireEvent.click(screen.getByRole("button", { name: "Move right →" }));

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());

    expect(sentTo(fetchStub)).toBe("/api/proxy/columns/c-todo/move");
    // An anchor, not an index: ADR 0004's ranks are not on the wire, and a
    // position would be a claim about a list this client last saw.
    expect(sentBody(fetchStub)).toEqual({ after_column_id: "c-doing" });
  });

  it("sends a null anchor when a column moves to the front", async () => {
    const fetchStub = respond(200, { column: columnBody("c-doing", "Doing") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("Doing");
    fireEvent.click(screen.getByRole("button", { name: "← Move left" }));

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    // "First" is the one position no sibling's id can name.
    expect(sentBody(fetchStub)).toEqual({ after_column_id: null });
  });

  it("re-reads the board rather than trusting its own splice", async () => {
    const fetchStub = respond(200, { column: columnBody("c-todo", "To do") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("To do");
    fireEvent.click(screen.getByRole("button", { name: "Move right →" }));

    // The board asks the server what the order is now. The optimistic reorder
    // was a display for the duration of the request and nothing more.
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("has no move at the ends of the board", () => {
    vi.stubGlobal("fetch", withRealtime(respond(200, {})));
    renderBoard();
    openColumnTools("To do");

    expect(screen.getByRole("button", { name: "← Move left" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Move right →" })).toBeEnabled();
  });

  it("ROLLBACK: puts the order back when the server refuses the move", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("To do");
    fireEvent.click(screen.getByRole("button", { name: "Move right →" }));

    await waitFor(() => expect(columnOrder()).toEqual(["Doing", "To do", "Done"]));

    // The stale-anchor 409 from `moveColumnHandler`: somebody else reordered
    // the board and the neighbour this client named is no longer next to it.
    settle(409, { error: "after_column_id is not another column on this board" });

    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Doing", "Done"]));
    expect(failureBanner()).toHaveTextContent(/changed while you were working on it/);
  });
});

describe("deleting a column", () => {
  it("says how many cards go with it, and sends nothing until confirmed", () => {
    const fetchStub = respond(204);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("Doing");
    fireEvent.click(screen.getByRole("button", { name: "Delete column" }));

    // The count is the fact that changes the answer.
    expect(screen.getByText("Delete Doing and its 3 cards?")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Delete column and 3 cards" }),
    ).toBeInTheDocument();
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("says so plainly when the column is empty", () => {
    vi.stubGlobal("fetch", withRealtime(respond(204)));
    renderBoard();
    openColumnTools("Done");
    fireEvent.click(screen.getByRole("button", { name: "Delete column" }));

    expect(screen.getByText("Delete Done?")).toBeInTheDocument();
    expect(screen.getByText(/This column is empty/)).toBeInTheDocument();
  });

  it("can be backed out of", () => {
    const fetchStub = respond(204);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("Doing");
    fireEvent.click(screen.getByRole("button", { name: "Delete column" }));
    fireEvent.click(screen.getByRole("button", { name: "Keep it" }));

    expect(columnOrder()).toEqual(["To do", "Doing", "Done"]);
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("deletes the column and its cards once confirmed", async () => {
    const fetchStub = respond(204);

    vi.stubGlobal("fetch", withRealtime(fetchStub));

    const view = renderBoard();

    openColumnTools("Doing");
    fireEvent.click(screen.getByRole("button", { name: "Delete column" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete column and 3 cards" }));

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(sentTo(fetchStub)).toBe("/api/proxy/columns/c-doing");
    expect(sentMethod(fetchStub)).toBe("DELETE");

    // The board is re-read rather than patched, so this is where the delete
    // actually becomes permanent.
    await waitFor(() => expect(refresh).toHaveBeenCalled());

    // The load-bearing assertion on this path: a 204 with no body was read as a
    // success. `DELETE` is the only endpoint here that answers with nothing, so
    // it is the only one whose success depends on `expectNoContent` and
    // `parseEmpty` agreeing — get that wrong and the delete "works", the row is
    // gone from the database, and the UI rolls back and reports a failure.
    expect(failureBanner()).toBeNull();

    // What `router.refresh()` produces in production: the Server Component runs
    // again and the page re-renders without the column or its cards.
    //
    // Asserting the *optimistic* disappearance here instead would be a race, and
    // was one — `refresh` is a bare `vi.fn()`, so nothing new ever arrives, and
    // on success React drops the optimistic value and re-renders from the
    // unchanged `snapshot` prop, which still has all three columns. With an
    // immediately-resolving `respond()` that window is already closed before
    // `waitFor` first samples the DOM, roughly three runs in eight under
    // full-suite load. The optimistic disappearance is covered by the ROLLBACK
    // test below, which holds the request open so the window is real.
    //
    // Same transition boundary as the add-card test above (#147), with the
    // opposite polarity and a milder failure. Here the unchanged snapshot still
    // HAS the Doing column, so once the transition ends it comes back -- which
    // means re-rendering mid-transition would let the final assertion pass
    // against the optimistic removal rather than against the re-rendered board.
    // That is a test passing for the wrong reason rather than a flake: quieter,
    // and no better. Waiting for the column to return makes what follows
    // unambiguous.
    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Doing", "Done"]));

    view.rerender(
      <BoardView
        boardId={BOARD}
        projectId={PROJECT}
        selectedCardId={null}
        snapshot={groupCardsIntoColumns(
          COLUMNS.filter((entry) => entry.id !== "c-doing") as never,
          CARDS.filter((entry) => entry.columnId !== "c-doing") as never,
        )}
      />,
    );

    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Done"]));
    expect(screen.queryByText("Zebra")).not.toBeInTheDocument();
    expect(screen.queryByText("Kilo")).not.toBeInTheDocument();
  });

  it("ROLLBACK: brings the column and every card in it back when the delete fails", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    openColumnTools("Doing");
    fireEvent.click(screen.getByRole("button", { name: "Delete column" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete column and 3 cards" }));

    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Done"]));
    expect(screen.queryByText("Zebra")).not.toBeInTheDocument();

    settle(404, { error: "column not found" });

    // The cards have to come back too. A rollback that restored the column and
    // lost its cards would be the worst outcome of the three.
    await waitFor(() => expect(columnOrder()).toEqual(["To do", "Doing", "Done"]));
    expect(screen.getByText("Zebra")).toBeInTheDocument();
    expect(screen.getByText("Kilo")).toBeInTheDocument();
    expect(screen.getByText("Alpha")).toBeInTheDocument();
    expect(failureBanner()).toHaveTextContent(/no longer exists/);
  });
});

describe("editing a card", () => {
  function openEditor() {
    renderBoard(COLUMNS, CARDS, "card-2");
    fireEvent.click(screen.getByRole("button", { name: "Edit card" }));
  }

  it("sends only the field that changed", async () => {
    const fetchStub = respond(200, { card: cardBody("card-2", "c-doing", "Kilo two", "Some detail") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openEditor();
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Kilo two" } });
    fireEvent.submit(screen.getByRole("button", { name: "Save card" }).closest("form")!);

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(sentTo(fetchStub)).toBe("/api/proxy/cards/card-2");
    // The untouched description is absent, not resent: PATCH leaves out what it
    // is not given, so sending a stale copy would overwrite a colleague's edit.
    expect(sentBody(fetchStub)).toEqual({ title: "Kilo two" });
  });

  it("can clear a description, because the API allows an empty one", async () => {
    const fetchStub = respond(200, { card: cardBody("card-2", "c-doing", "Kilo") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openEditor();
    fireEvent.change(screen.getByLabelText(/Description/), { target: { value: "" } });
    fireEvent.submit(screen.getByRole("button", { name: "Save card" }).closest("form")!);

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(sentBody(fetchStub)).toEqual({ description: "" });
  });

  it("closes without a request when nothing was changed", () => {
    const fetchStub = respond(200, {});

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openEditor();
    fireEvent.submit(screen.getByRole("button", { name: "Save card" }).closest("form")!);

    // The handler answers 400 to a body mentioning neither field.
    expect(fetchStub).not.toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: "Kilo" })).toBeInTheDocument();
  });

  it("refuses an over-long description without sending anything", () => {
    const fetchStub = respond(200, {});

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openEditor();
    fireEvent.change(screen.getByLabelText(/Description/), {
      target: { value: "x".repeat(10_001) },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Save card" }).closest("form")!);

    // 10,000 is `maxDescriptionLength` in crud.go.
    expect(
      screen.getByText("Descriptions can be at most 10000 characters."),
    ).toBeInTheDocument();
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("refuses an emptied title, which PATCH rejects where it accepts an empty description", () => {
    const fetchStub = respond(200, {});

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openEditor();
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "  " } });
    fireEvent.submit(screen.getByRole("button", { name: "Save card" }).closest("form")!);

    expect(screen.getByText("Give the card a title.")).toBeInTheDocument();
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("ROLLBACK: puts the old title back on the tile when the save is refused", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openEditor();
    fireEvent.change(screen.getByLabelText("Title"), { target: { value: "Kilo two" } });
    fireEvent.submit(screen.getByRole("button", { name: "Save card" }).closest("form")!);

    // The tile behind the panel is the thing to watch: it is drawn from the
    // board's own optimistic store, which is the state under test.
    const doing = await screen.findByRole("list", { name: "Doing" });

    await waitFor(() => expect(within(doing).getByText("Kilo two")).toBeInTheDocument());

    settle(404, { error: "card not found" });

    await waitFor(() =>
      expect(within(doing).queryByText("Kilo two")).not.toBeInTheDocument(),
    );
    expect(within(doing).getByText("Kilo")).toBeInTheDocument();
    expect(failureBanner()).toHaveTextContent(/no longer exists/);
  });
});

describe("deleting a card", () => {
  function openDeleteConfirm() {
    renderBoard(COLUMNS, CARDS, "card-2");
    fireEvent.click(screen.getByRole("button", { name: "Delete card" }));
  }

  it("confirms before deleting, and sends nothing until then", () => {
    const fetchStub = respond(204);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openDeleteConfirm();

    expect(screen.getByText(/cannot be undone/)).toBeInTheDocument();
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("deletes the card and closes the panel", async () => {
    const fetchStub = respond(204);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openDeleteConfirm();
    fireEvent.click(screen.getByRole("button", { name: "Delete card" }));

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());
    expect(sentTo(fetchStub)).toBe("/api/proxy/cards/card-2");
    expect(sentMethod(fetchStub)).toBe("DELETE");

    // Back to the board without `?card=`, because the card it named is gone.
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith("/app/projects/p-1/boards/b-1"),
    );
  });

  it("ROLLBACK: puts the card back on the board when the delete is refused", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    openDeleteConfirm();
    fireEvent.click(screen.getByRole("button", { name: "Delete card" }));

    const doing = await screen.findByRole("list", { name: "Doing" });

    await waitFor(() => expect(within(doing).queryByText("Kilo")).not.toBeInTheDocument());

    settle(500, { error: "internal server error" });

    await waitFor(() => expect(within(doing).getByText("Kilo")).toBeInTheDocument());
    expect(failureBanner()).toHaveTextContent(/Something went wrong/);
    expect(push).not.toHaveBeenCalled();
  });
});

describe("the board's read-only rendering", () => {
  it("still shows no editing controls for a column the server has not confirmed", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();
    fireEvent.click(screen.getByRole("button", { name: "+ Add a column" }));
    fireEvent.change(screen.getByLabelText("New column name"), {
      target: { value: "Blocked" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add column" }).closest("form")!);

    await waitFor(() => expect(columnOrder()).toContain("Blocked"));

    // It has no id to address yet, so offering Edit would be offering a 404.
    expect(screen.queryByRole("button", { name: "Edit Blocked" })).not.toBeInTheDocument();

    settle(201, { column: columnBody("c-new", "Blocked") });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });
});
