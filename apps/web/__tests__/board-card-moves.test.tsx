/**
 * Moving a card: what goes on the wire, in what order, and what happens when
 * the server refuses it.
 *
 * The rules `board-editing.test.tsx` sets out apply here unchanged, and the one
 * about the optimistic window is the one to re-read before adding to this file:
 * `refresh` is a bare `vi.fn()`, so on success the transition ends, React
 * discards the optimistic value, and the board re-renders from the *unchanged*
 * `snapshot` prop. Asserting optimistic DOM after an immediately-resolving
 * `respond(...)` is therefore a race that passes most of the time. Each
 * assertion below is one of the three honest shapes: {@link pending} for "it
 * showed before the answer", the stub for "the right request went out", and
 * `respond` + `refresh` + `rerender` for "the change stuck".
 *
 * # What is not tested here, and why
 *
 * The pointer gesture. jsdom has no layout, so every `getBoundingClientRect` is
 * zeroes and no drag library — or hand-written equivalent — can be driven in
 * it. Rather than assert against a fake, the gesture is reduced to two facts
 * (*what is the pointer over*, *which side of it*) in `card-drag.tsx`, and the
 * function those two facts feed, `cardDropTarget`, is exhaustively tested in
 * `board-mutations.test.ts`. What is left untested by machine is the wiring
 * between dnd-kit's events and that call, which is the part exercised by hand
 * against a real API — see the PR.
 *
 * The keyboard path is the same code from `cardNudge` onwards, so what these
 * tests drive is the whole of the client's move behaviour bar the gesture.
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

function card(id: string, columnId: string, title: string) {
  return {
    id,
    boardId: BOARD,
    columnId,
    title,
    description: "",
    assigneeId: null,
    dueAt: null,
    createdAt: "2026-08-02T09:00:00Z",
    updatedAt: "2026-08-02T09:00:00Z",
  };
}

/**
 * The workspace's members, as `GET /members` returns them.
 *
 * Two, with different initials, because one member cannot tell an avatar that
 * resolved the right name from one that resolved the only name there was.
 */
const MEMBERS = [
  {
    membershipId: "mem-1",
    userId: "user-dana",
    email: "dana@example.test",
    displayName: "Dana Okoro",
    role: "admin",
    joinedAt: "2026-07-01T09:00:00Z",
  },
  {
    membershipId: "mem-2",
    userId: "user-sam",
    email: "sam@example.test",
    displayName: "Sam Ito",
    role: "member",
    joinedAt: "2026-07-02T09:00:00Z",
  },
];

function cardBody(id: string, columnId: string, title: string) {
  return {
    id,
    board_id: BOARD,
    column_id: columnId,
    title,
    description: "",
    assignee_id: null,
    due_at: null,
    created_at: "2026-08-02T09:00:00Z",
    updated_at: "2026-08-02T09:00:00Z",
  };
}

const COLUMNS = [column("c-todo", "To do"), column("c-doing", "Doing"), column("c-done", "Done")];

// Doing holds three, To do holds one, Done is empty — so a move within a
// column, a move into an occupied one and a move into an empty one are all
// reachable from one fixture.
const CARDS = [
  card("card-3", "c-doing", "Zebra"),
  card("card-2", "c-doing", "Kilo"),
  card("card-1", "c-doing", "Alpha"),
  card("card-4", "c-todo", "Mike"),
];

function renderBoard(cards = CARDS, columns = COLUMNS) {
  return render(
    <BoardView
      boardId={BOARD}
      members={MEMBERS}
      projectId={PROJECT}
      selectedCardId={null}
      snapshot={groupCardsIntoColumns(columns as never, cards as never)}
    />,
  );
}

/** A `fetch` that hangs until the test settles it. */
function pending() {
  let release!: (response: Response) => void;

  const promise = new Promise<Response>((resolve) => {
    release = resolve;
  });

  return {
    fetchStub: vi.fn<FetchStub>(() => promise),
    settle: (status: number, body: unknown = {}) =>
      release(
        new Response(JSON.stringify(body), {
          status,
          headers: { "content-type": "application/json" },
        }),
      ),
  };
}

/**
 * A `fetch` that hands out a *separate* held promise per call.
 *
 * {@link pending} returns one promise to every caller, which cannot express the
 * thing this file most needs to say: that two requests were outstanding at once
 * and were answered in a particular order. Two sequential awaited moves would
 * pass against a client with no ordering at all.
 */
function concurrent() {
  const releases: ((response: Response) => void)[] = [];

  return {
    fetchStub: vi.fn<FetchStub>(
      () => new Promise<Response>((resolve) => releases.push(resolve)),
    ),
    settle: (call: number, status: number, body: unknown = {}) =>
      releases[call](
        new Response(JSON.stringify(body), {
          status,
          headers: { "content-type": "application/json" },
        }),
      ),
  };
}

function respond(status: number, body: unknown = {}) {
  return vi.fn<FetchStub>(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json" },
      }),
  );
}

type Stub = ReturnType<typeof respond>;

function sentBody(stub: Stub, call = 0): unknown {
  return JSON.parse(String(stub.mock.calls[call][1]?.body));
}

function sentTo(stub: Stub, call = 0): string {
  return String(stub.mock.calls[call][0]);
}

/** The grip that moves one card, which is also its keyboard handle. */
function grip(title: string) {
  return screen.getByRole("button", { name: `Move ${title}` });
}

/**
 * Picks a card up, and puts it down.
 *
 * A **click**, not a keydown, because that is what the grip listens for and
 * that is the whole point: Enter and Space are left to become a click so that
 * the assistive technologies which activate a button by calling `click()` —
 * screen readers in browse mode, voice control, a VoiceOver double-tap — reach
 * the same code. Driving this with `keyDown` would test a path no user has.
 */
function lift(title: string) {
  fireEvent.click(grip(title));
}

const drop = lift;

/** An arrow key or Escape, which are the only keys the grip handles itself. */
function press(title: string, key: string) {
  fireEvent.keyDown(grip(title), { key });
}

/** A column's cards, top to bottom, without the grips' own labels. */
function order(columnName: string): string[] {
  return within(screen.getByRole("list", { name: columnName }))
    .getAllByRole("listitem")
    .map((li) => within(li).getByRole("link").textContent ?? "");
}

/**
 * The live region this board owns.
 *
 * By name, because `DndContext` renders one of its own that this board keeps
 * silent — an unnamed `getByRole("status")` finds both.
 */
function moveRegion(): HTMLElement {
  return screen.getByRole("status", { name: "Card moves" });
}

/** What the board is telling a screen reader right now. */
function announced(): string {
  // Zero-width spaces are how a repeated sentence is made a new string; they
  // are not part of what is said.
  return (moveRegion().textContent ?? "").replace(/​/g, "");
}

beforeEach(() => {
  push.mockClear();
  refresh.mockClear();
  vi.unstubAllGlobals();
  __resetBrowserApiForTests();
});

describe("what a move puts on the wire", () => {
  it("posts an anchor to the card's own move endpoint, through the proxy", async () => {
    const fetchStub = respond(200, { card: cardBody("card-3", "c-doing", "Zebra") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());

    expect(sentTo(fetchStub)).toBe("/api/proxy/cards/card-3/move");
    // Zebra was first and went to second, so it now follows Kilo. An index
    // would have said "1"; this says which row, which is what survives someone
    // else reordering the column underneath it.
    expect(sentBody(fetchStub)).toEqual({
      column_id: "c-doing",
      after_card_id: "card-2",
    });
  });

  it("never sends a rank or a position, in any move it can make", async () => {
    // ADR 0004's third decision, asserted rather than assumed: no endpoint
    // accepts a position and no client may invent one. The body has exactly two
    // keys and this is the test that fails if a third ever appears.
    const fetchStub = respond(200, { card: cardBody("card-1", "c-todo", "Alpha") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Alpha");
    press("Alpha", "ArrowLeft");
    drop("Alpha");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());

    expect(Object.keys(sentBody(fetchStub) as object).sort()).toEqual([
      "after_card_id",
      "column_id",
    ]);
  });

  it("sends a null anchor for the top of a column, not an id it made up", async () => {
    const fetchStub = respond(200, { card: cardBody("card-2", "c-doing", "Kilo") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Kilo");
    press("Kilo", "ArrowUp");
    drop("Kilo");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());

    expect(sentBody(fetchStub)).toEqual({
      column_id: "c-doing",
      after_card_id: null,
    });
  });

  it("names the target column when the card crosses into an empty one", async () => {
    const fetchStub = respond(200, { card: cardBody("card-1", "c-done", "Alpha") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Alpha");
    press("Alpha", "ArrowRight");
    drop("Alpha");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());

    expect(sentBody(fetchStub)).toEqual({
      column_id: "c-done",
      after_card_id: null,
    });
  });

  it("sends one request for a move of several places, not one per keypress", async () => {
    // The reason the keyboard proposes rather than commits. Five presses that
    // each posted would be five whole-board re-reads and five chances for
    // somebody else's edit to land in the middle of one gesture.
    const fetchStub = respond(200, { card: cardBody("card-3", "c-todo", "Zebra") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    press("Zebra", "ArrowDown");
    press("Zebra", "ArrowLeft");
    drop("Zebra");

    await waitFor(() => expect(fetchStub).toHaveBeenCalled());

    expect(fetchStub).toHaveBeenCalledTimes(1);
    expect(sentBody(fetchStub)).toEqual({
      column_id: "c-todo",
      after_card_id: "card-4",
    });
  });

  it("sends nothing for a lift that was cancelled", () => {
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    press("Zebra", "Escape");

    expect(fetchStub).not.toHaveBeenCalled();
    expect(order("Doing")).toEqual(["Zebra", "Kilo", "Alpha"]);
  });

  it("sends nothing for a move that puts the card back where it started", () => {
    // Down then up. `after_card_id` equal to the card's own id is a 409 rather
    // than a no-op, so a client that posted this would be manufacturing errors.
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    press("Zebra", "ArrowUp");
    drop("Zebra");

    expect(fetchStub).not.toHaveBeenCalled();
  });
});

describe("the board while the server is deciding", () => {
  it("shows the card in its new place before the answer arrives", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    await waitFor(() => expect(order("Doing")).toEqual(["Kilo", "Zebra", "Alpha"]));

    settle(200, { card: cardBody("card-3", "c-doing", "Zebra") });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("moves the card between columns before the answer arrives", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Alpha");
    press("Alpha", "ArrowLeft");
    drop("Alpha");

    await waitFor(() => expect(order("To do")).toEqual(["Mike", "Alpha"]));
    expect(order("Doing")).toEqual(["Zebra", "Kilo"]);

    settle(200, { card: cardBody("card-1", "c-todo", "Alpha") });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("keeps the move once the server has confirmed it and the board is re-read", async () => {
    const fetchStub = respond(200, { card: cardBody("card-3", "c-doing", "Zebra") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    const { rerender } = renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    await waitFor(() => expect(refresh).toHaveBeenCalled());

    // What `router.refresh()` does in production: the Server Component re-runs
    // and a new prop arrives. Asserting the optimistic DOM instead would be
    // asserting a window that has already closed.
    rerender(
      <BoardView
        boardId={BOARD}
        members={MEMBERS}
        projectId={PROJECT}
        selectedCardId={null}
        snapshot={
          groupCardsIntoColumns(COLUMNS as never, [
            card("card-2", "c-doing", "Kilo"),
            card("card-3", "c-doing", "Zebra"),
            card("card-1", "c-doing", "Alpha"),
            card("card-4", "c-todo", "Mike"),
          ] as never)
        }
      />,
    );

    expect(order("Doing")).toEqual(["Kilo", "Zebra", "Alpha"]);
  });
});

describe("two moves of one card, both in flight", () => {
  it("holds the second request until the first has been answered", async () => {
    const { fetchStub, settle } = concurrent();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    // Zebra down one: first → second, so it lands after Kilo.
    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(1));

    // Down again, while the first is still unanswered. The anchor is computed
    // from the board with the first move already applied, so it names Alpha.
    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    await waitFor(() => expect(order("Doing")).toEqual(["Kilo", "Alpha", "Zebra"]));

    // The whole point: the second move is on screen but not on the wire. If
    // both flew now, the server would apply them in arrival order — ADR 0004's
    // last-writer-wins is per *card*, so the loser is whichever request the
    // network happened to deliver second, not whichever the user made second.
    expect(fetchStub).toHaveBeenCalledTimes(1);
    expect(sentBody(fetchStub, 0)).toEqual({
      column_id: "c-doing",
      after_card_id: "card-2",
    });

    settle(0, 200, { card: cardBody("card-3", "c-doing", "Zebra") });

    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(2));

    // Second out, second applied, and a different anchor — which is what makes
    // the order observable at all. Had they raced and arrived the other way
    // round, the board would have settled with Zebra second.
    expect(sentBody(fetchStub, 1)).toEqual({
      column_id: "c-doing",
      after_card_id: "card-1",
    });

    settle(1, 200, { card: cardBody("card-3", "c-doing", "Zebra") });
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
  });

  it("does not make one card's move wait for another card's", async () => {
    // Different rows, disjoint at the database too — the queue is per card
    // because that is the granularity the fractional ranks make independent.
    const { fetchStub, settle } = concurrent();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    lift("Mike");
    press("Mike", "ArrowRight");
    drop("Mike");

    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(2));

    expect(sentTo(fetchStub, 0)).toBe("/api/proxy/cards/card-3/move");
    expect(sentTo(fetchStub, 1)).toBe("/api/proxy/cards/card-4/move");

    settle(0, 200, { card: cardBody("card-3", "c-doing", "Zebra") });
    settle(1, 200, { card: cardBody("card-4", "c-doing", "Mike") });
    await waitFor(() => expect(refresh).toHaveBeenCalledTimes(2));
  });

  it("abandons the moves queued behind one the server refused", async () => {
    // The subtle half of the queue. A queued move was computed against a board
    // with its predecessor applied, and it usually *carries* that predecessor:
    // move a card into Done and then nudge it within Done, and the second move
    // names Done too. Sending it after the first was refused would perform the
    // move the user has just been told did not happen.
    const { fetchStub, settle } = concurrent();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Alpha");
    press("Alpha", "ArrowRight");
    drop("Alpha");

    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(1));
    expect(sentBody(fetchStub, 0)).toEqual({
      column_id: "c-done",
      after_card_id: null,
    });

    // Queued behind the first, while it is still unanswered.
    lift("Alpha");
    press("Alpha", "ArrowUp");
    drop("Alpha");

    settle(0, 409, { error: "after_card_id is not a card in that column" });

    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeNull());

    // The second never goes out, and there is one explanation rather than two.
    expect(fetchStub).toHaveBeenCalledTimes(1);
    await waitFor(() => expect(order("Doing")).toEqual(["Zebra", "Kilo", "Alpha"]));
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  });

  it("lets a card be moved again after one of its moves was refused", async () => {
    // The queue must drain on failure too, or one 409 makes a card permanently
    // unmovable for the life of the page.
    const { fetchStub, settle } = concurrent();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(1));
    settle(0, 409, { error: "after_card_id is not a card in that column" });

    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeNull());

    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");

    await waitFor(() => expect(fetchStub).toHaveBeenCalledTimes(2));

    settle(1, 200, { card: cardBody("card-3", "c-doing", "Zebra") });
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });
});

describe("a stale anchor", () => {
  it("puts the card back, re-reads the board, and says why", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Alpha");
    press("Alpha", "ArrowUp");
    drop("Alpha");

    // On screen first, so this fails against a board that was never optimistic
    // as loudly as against one that never puts the card back.
    await waitFor(() => expect(order("Doing")).toEqual(["Zebra", "Alpha", "Kilo"]));

    settle(409, { error: "after_card_id is not a card in that column" });

    await waitFor(() => expect(order("Doing")).toEqual(["Zebra", "Kilo", "Alpha"]));

    // Not the generic 409 sentence: that one asks for a reload this has already
    // done, and does not say the card went back — which is the fact that
    // decides whether the user drags again or goes looking for their card.
    expect(screen.getByRole("alert")).toHaveTextContent(
      /back where it started and the board has been refreshed/,
    );

    // Re-read rather than retried. A retry would have to pick a new anchor from
    // the refreshed board, and "the second slot" is an index — the claim ADR
    // 0004 refuses, because the server cannot tell it from a stale one.
    expect(refresh).toHaveBeenCalled();
    expect(fetchStub).toHaveBeenCalledTimes(1);
  });

  it("says the ordinary thing for a failure that is not a conflict", async () => {
    const { fetchStub, settle } = pending();

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Alpha");
    press("Alpha", "ArrowUp");
    drop("Alpha");

    await waitFor(() => expect(order("Doing")).toEqual(["Zebra", "Alpha", "Kilo"]));

    settle(500, { error: "internal server error" });

    await waitFor(() => expect(order("Doing")).toEqual(["Zebra", "Kilo", "Alpha"]));
    expect(screen.getByRole("alert")).toHaveTextContent(
      /could not move this card/,
    );
    expect(refresh).not.toHaveBeenCalled();
  });
});

describe("saying where the card went", () => {
  it("announces the position on every keypress, because nothing else does", () => {
    // A reorder fires no accessibility event. Without this the whole feature is
    // silent for anyone who cannot see the card move.
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    expect(announced()).toMatch(/Zebra lifted\. Doing, 1 of 3\./);

    press("Zebra", "ArrowDown");
    expect(announced()).toBe("Doing, 2 of 3.");

    press("Zebra", "ArrowLeft");
    expect(announced()).toBe("To do, 2 of 2.");
  });

  it("says so when the card cannot go any further", () => {
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowUp");

    expect(announced()).toMatch(/Zebra cannot move up from here\. Doing, 1 of 3\./);
  });

  it("says where the card ended up, and where it went back to", () => {
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    drop("Zebra");
    expect(announced()).toMatch(/Zebra moved\. Doing, 2 of 3\./);

    lift("Kilo");
    press("Kilo", "ArrowDown");
    press("Kilo", "Escape");
    expect(announced()).toMatch(/Move cancelled\. Kilo is back in Doing, 1 of 3\./);
  });

  it("says so when a card is lifted and dropped without being moved", () => {
    // For someone who cannot see the card, an unremarked drop is
    // indistinguishable from a key that did nothing — and the next arrow press
    // would be aimed at a card they no longer hold.
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Kilo");
    drop("Kilo");

    expect(announced()).toMatch(/Kilo was not moved\. Doing, 2 of 3\./);
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("repeats itself audibly when the answer is the same twice", () => {
    // A live region whose text has not changed is not re-announced, so two
    // presses that both hit the top of a column would be one announcement and
    // then silence — indistinguishable from the key not working.
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowUp");

    const first = moveRegion().textContent;

    press("Zebra", "ArrowUp");

    expect(moveRegion().textContent).not.toBe(first);
    expect(announced()).toMatch(/cannot move up from here/);
  });
});

describe("the grip, as a control", () => {
  it("is activated by a click, not only by a physical keypress", () => {
    // The grip handles no Enter or Space of its own, so that the browser's
    // synthesised click gets through. That click is the only thing a screen
    // reader in browse mode, a voice-control command, or a VoiceOver double-tap
    // produces — handling the keydown and calling preventDefault would leave
    // this button inert for every one of them, which on touch is the whole
    // feature gone.
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    fireEvent.click(grip("Zebra"));

    expect(grip("Zebra")).toHaveAttribute("aria-pressed", "true");
    expect(announced()).toMatch(/Zebra lifted\./);
  });

  it("reports itself pressed only while the card is held", () => {
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    expect(grip("Zebra")).toHaveAttribute("aria-pressed", "false");

    lift("Zebra");
    expect(grip("Zebra")).toHaveAttribute("aria-pressed", "true");

    press("Zebra", "Escape");
    expect(grip("Zebra")).toHaveAttribute("aria-pressed", "false");
  });

  it("keeps the focus on the grip when the card changes column", () => {
    // The bug this guards was invisible to every other test and cost a real
    // browser session to find: moving a card into another column re-parents its
    // row, so React unmounts the grip and focus falls to the document. Every
    // key after the one that crossed the column then landed nowhere, and the
    // move was announced correctly and never sent.
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    grip("Alpha").focus();
    lift("Alpha");
    press("Alpha", "ArrowLeft");

    expect(order("To do")).toEqual(["Mike", "Alpha"]);
    expect(document.activeElement).toBe(grip("Alpha"));

    // And the next key still reaches it, which is the fact that actually broke.
    press("Alpha", "ArrowUp");
    expect(order("To do")).toEqual(["Alpha", "Mike"]);
  });

  it("gives the move up when focus leaves for another control", () => {
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    press("Zebra", "ArrowDown");
    fireEvent.focusOut(grip("Zebra"), { relatedTarget: document.body });

    expect(announced()).toMatch(/Move cancelled\./);
    expect(grip("Zebra")).toHaveAttribute("aria-pressed", "false");
    expect(fetchStub).not.toHaveBeenCalled();
  });

  it("does not give it up when focus goes nowhere, which is a re-parent", () => {
    // A null `relatedTarget` is the grip being unmounted as the card moves
    // column — the effect above is mid-repair. Cancelling on it would end every
    // cross-column move in the act of making it.
    const fetchStub = respond(200);

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    lift("Zebra");
    fireEvent.focusOut(grip("Zebra"), { relatedTarget: null });

    expect(grip("Zebra")).toHaveAttribute("aria-pressed", "true");
    expect(announced()).not.toMatch(/cancelled/);
  });
});

describe("what cannot be moved", () => {
  it("gives no grip to a card the server has not acknowledged", () => {
    const fetchStub = respond(201, { card: cardBody("card-new", "c-done", "Fresh") });

    vi.stubGlobal("fetch", withRealtime(fetchStub));
    renderBoard();

    fireEvent.click(screen.getByRole("button", { name: "Add a card to Done" }));
    fireEvent.change(screen.getByLabelText("New card in Done"), {
      target: { value: "Fresh" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    // Its id was invented by this client and is not a uuid, so a move naming it
    // — or anchored on it — is a 400 waiting to happen.
    expect(screen.queryByRole("button", { name: "Move Fresh" })).toBeNull();
  });
});
