/**
 * The board, live: somebody else's change arriving while you are looking at it.
 *
 * # The rule this file pins down
 *
 * The board is drawn from three layers — the Server Component's snapshot, then
 * everyone else's events, then this user's own optimistic edits, in that order.
 * The ordering is the whole answer to the flicker question the issue warns
 * about: an inbound `card.moved` for a card you are currently moving lands
 * *underneath* your own move, which is replayed on top, so the card does not
 * jump out from under you. That is asserted here rather than argued for in a
 * comment, because it is the behaviour a live demo shows off and the one a
 * refactor would quietly break.
 *
 * # No real sockets, and every real timer is bounded and deterministic
 *
 * `support/realtime.ts` is a stream this test enqueues into directly, so "the
 * server sent this event" is a synchronous call, and nothing below asserts a
 * transient window against an immediately-resolving stub.
 *
 * Two real timers *are* involved, and an earlier version of this header claimed
 * neither was. Saying so cost a flake that ran for a while before it was
 * characterised, so they are both named here:
 *
 * 1. **The coalesced re-read**, a fixed 120 ms. Only ever observed through
 *    `refresh` having been called, which is a mock call and therefore permanent
 *    once it happens — the assertion shape `board-editing.test.tsx`'s header
 *    says is safe.
 * 2. **The reconnect backoff**, which is the one that bit. It is exponential
 *    with *full jitter*: a 1006 close escalates the attempt counter, so the wait
 *    is `max(100, round(1000 × Math.random()))` — anywhere from 100 ms to a full
 *    second. `waitFor`'s default budget is also one second, so on a high draw
 *    under full-suite load the reconnect assertion had no headroom at all and
 *    failed roughly one run in seven.
 *
 * **`Math.random` is therefore pinned to 0 for this whole file**, which puts
 * every backoff on its 100 ms floor. That removes the race rather than making it
 * rarer, which a longer timeout would not have done: a widened budget is still
 * probabilistic, it just relocates the failure to a machine nobody is watching.
 * The jitter's *distribution* is a property of `backoffDelayMs` and is tested
 * where it belongs, in `realtime-recovery.test.ts`, against an injected `random`.
 *
 * Pinning also gives the "stops trying after a 4003" test teeth it did not have.
 * It asserts that no reconnect happens within 250 ms; with a random 100–1000 ms
 * backoff, a client that *did* wrongly retry would often have been scheduled
 * past that window and the test would have passed anyway. At a fixed 100 ms, a
 * wrongful retry lands well inside it.
 */

import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { fakeRealtime } from "./support/realtime";

const push = vi.fn();
const refresh = vi.fn();

vi.mock("next/navigation", () => ({ useRouter: () => ({ push, refresh }) }));

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
const OTHER_BOARD = "b-2";

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
    createdAt: "2026-08-02T09:00:00Z",
    updatedAt: "2026-08-02T09:00:00Z",
  };
}

function cardBody(id: string, columnId: string, title: string, boardId = BOARD) {
  return {
    id,
    board_id: boardId,
    column_id: columnId,
    title,
    description: "",
    created_at: "2026-08-02T09:00:00Z",
    updated_at: "2026-08-02T09:00:00Z",
  };
}

function columnBody(id: string, name: string) {
  return {
    id,
    board_id: BOARD,
    name,
    created_at: "2026-08-01T09:00:00Z",
    updated_at: "2026-08-01T09:00:00Z",
  };
}

/**
 * A counter for event ids.
 *
 * `Math.random()` is pinned to 0 for this file, so it can no longer make a
 * distinct id — and an id is the one field the server guarantees is unique per
 * event. Nothing here depends on that yet, because the live log does not dedupe
 * on it, but a fixture that silently emits the same id for every event is a trap
 * for whoever adds the test that does.
 */
let nextEventId = 0;

/** An `event` frame exactly as the Go API writes it. */
function event(type: string, payload: unknown, boardId = BOARD) {
  nextEventId += 1;

  return {
    type: "event",
    board_id: boardId,
    event: {
      id: `e-${nextEventId}`,
      type,
      actor_id: "someone-else",
      occurred_at: "2026-08-10T01:19:58.424746926Z",
      payload,
    },
  };
}

const COLUMNS = [column("c-todo", "To do"), column("c-doing", "Doing"), column("c-done", "Done")];

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
      projectId={PROJECT}
      selectedCardId={null}
      snapshot={groupCardsIntoColumns(columns as never, cards as never)}
    />,
  );
}

function respond(status: number, body: unknown = {}) {
  return vi.fn<FetchStub>(
    async () =>
      new Response(status === 204 ? null : JSON.stringify(body), {
        status,
        headers: { "content-type": "application/json" },
      }),
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

/** A column's cards, top to bottom. */
function order(columnName: string): string[] {
  return within(screen.getByRole("list", { name: columnName }))
    .getAllByRole("listitem")
    .map((li) => {
      const link = within(li).queryByRole("link");

      return (link ?? li).textContent ?? "";
    });
}

function grip(title: string) {
  return screen.getByRole("button", { name: `Move ${title}` });
}

/** Opens the stream and gets past the subscribe, which is where "live" starts. */
async function goLive(live: ReturnType<typeof fakeRealtime>) {
  await waitFor(() => expect(live.opened()).toBe(1));

  live.subscribe(BOARD);

  await screen.findByText("Live");
}

beforeEach(() => {
  push.mockClear();
  refresh.mockClear();
  vi.unstubAllGlobals();
  __resetBrowserApiForTests();

  // Pins every reconnect backoff to its 100 ms floor. See the note on timers in
  // this file's header: without it the first reconnect waits a uniformly random
  // 100–1000 ms against `waitFor`'s 1000 ms budget, which is not a slow test but
  // a race, and it lost about one run in seven under full-suite load.
  vi.spyOn(Math, "random").mockReturnValue(0);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("re-reading the board on every subscribe", () => {
  it("re-reads as soon as it subscribes, because the props predate the socket", async () => {
    // ADR 0005: Redis pub/sub holds nothing, so anything published between the
    // Server Component's read and this subscription is gone and nothing will
    // resend it. The re-fetch is the recovery design, not an extra.
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();

    await goLive(live);
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("re-reads again after a reconnect, which is the disconnected-change case", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();

    await goLive(live);
    await waitFor(() => expect(refresh).toHaveBeenCalled());

    const before = refresh.mock.calls.length;

    // The connection drops and comes back. Whatever changed while it was down
    // was never delivered, so only a full read can find it.
    live.close(1006, "connection lost");

    await waitFor(() => expect(live.opened()).toBe(2));

    live.subscribe(BOARD);

    await waitFor(() => expect(refresh.mock.calls.length).toBeGreaterThan(before));
  });
});

describe("applying somebody else's change", () => {
  it("moves a card when the server says it moved", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    expect(order("Doing")).toEqual(["Zebra", "Kilo", "Alpha"]);

    live.frame(
      event("card.moved", {
        card: cardBody("card-1", "c-doing", "Alpha"),
        from_column_id: "c-doing",
        after_card_id: null,
      }),
    );

    // Applied, not re-sorted: the anchor the server named decides the position.
    await waitFor(() => expect(order("Doing")).toEqual(["Alpha", "Zebra", "Kilo"]));
  });

  it("moves a card into another column", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    live.frame(
      event("card.moved", {
        card: cardBody("card-3", "c-done", "Zebra"),
        from_column_id: "c-doing",
        after_card_id: null,
      }),
    );

    await waitFor(() => expect(order("Done")).toEqual(["Zebra"]));
    expect(order("Doing")).toEqual(["Kilo", "Alpha"]);
  });

  it("renames a card, and removes a deleted one", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    live.frame(event("card.updated", { card: cardBody("card-2", "c-doing", "Kilo, renamed") }));
    await waitFor(() => expect(order("Doing")).toContain("Kilo, renamed"));

    live.frame(event("card.deleted", { card_id: "card-3", column_id: "c-doing" }));
    await waitFor(() => expect(order("Doing")).toEqual(["Kilo, renamed", "Alpha"]));
  });

  it("adds a column somebody else created", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    live.frame(event("column.created", { column: columnBody("c-new", "Blocked") }));

    await screen.findByRole("heading", { name: "Blocked" });
  });

  it("takes a deleted column's cards with it, as the database does", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    live.frame(event("column.deleted", { column_id: "c-doing" }));

    await waitFor(() =>
      expect(screen.queryByRole("heading", { name: "Doing" })).not.toBeInTheDocument(),
    );
    expect(screen.queryByText("Zebra")).not.toBeInTheDocument();
  });

  it("never applies an event for a board this screen is not showing", async () => {
    // The relay subscribes to one board, so this is not reachable in
    // production. It is checked at both ends anyway: a delivered event is
    // applied without further authorisation, so "which board is this for" is
    // not a question to answer only once.
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    live.frame(
      event(
        "card.deleted",
        { card_id: "card-3", column_id: "c-doing" },
        OTHER_BOARD,
      ),
    );

    // Give the frame every chance to be applied before concluding it was not.
    await waitFor(() => expect(refresh).toHaveBeenCalled());
    expect(order("Doing")).toEqual(["Zebra", "Kilo", "Alpha"]);
  });
});

describe("reconciling with this user's own edits", () => {
  it("does not move a card out from under the person moving it", async () => {
    // The flicker case. A move is in flight — the request is held open, so the
    // optimistic position is genuinely on screen rather than momentarily — and
    // the server announces a *different* position for the same card. The
    // optimistic layer sits above the live layer, so what the user is looking
    // at is what they did.
    const { fetchStub, settle } = pending();
    const live = fakeRealtime(fetchStub);

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    // Lift Alpha and move it to the top of Doing.
    fireEvent.click(grip("Alpha"));
    fireEvent.keyDown(grip("Alpha"), { key: "ArrowUp" });
    fireEvent.keyDown(grip("Alpha"), { key: "ArrowUp" });
    fireEvent.click(grip("Alpha"));

    await waitFor(() => expect(order("Doing")).toEqual(["Alpha", "Zebra", "Kilo"]));

    // Somebody else's move of the same card arrives mid-flight.
    live.frame(
      event("card.moved", {
        card: cardBody("card-1", "c-done", "Alpha"),
        from_column_id: "c-doing",
        after_card_id: null,
      }),
    );

    await waitFor(() => expect(refresh).toHaveBeenCalled());

    // Still where this user put it. No jump, and exactly one of it — the live
    // layer moved it to Done underneath, and the optimistic layer put it back.
    expect(order("Doing")).toEqual(["Alpha", "Zebra", "Kilo"]);
    expect(screen.getAllByText("Alpha")).toHaveLength(1);

    settle(200, { card: cardBody("card-1", "c-doing", "Alpha") });
  });

  it("does not draw a card twice when the user's own create comes back", async () => {
    // A pending card carries a `pending:` id that cannot be matched to the
    // server's row — that is what the prefix is for. So while a create is
    // unconfirmed, an inbound `card.created` is left to the re-read, which
    // replaces the placeholder and adds the real row in one step.
    const { fetchStub } = pending();
    const live = fakeRealtime(fetchStub);

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    fireEvent.click(screen.getByRole("button", { name: "Add a card to Done" }));
    fireEvent.change(screen.getByLabelText("New card in Done"), {
      target: { value: "Fresh" },
    });
    fireEvent.submit(screen.getByRole("button", { name: "Add card" }).closest("form")!);

    // The placeholder reads "FreshAdding…" — it is deliberately not a link,
    // because its id was invented by this client and names nothing yet.
    await waitFor(() =>
      expect(order("Done").filter((title) => title.includes("Fresh"))).toHaveLength(1),
    );

    // The server's version of that same card arrives while the POST is still
    // in flight.
    live.frame(event("card.created", { card: cardBody("card-new", "c-done", "Fresh") }));

    await waitFor(() => expect(refresh).toHaveBeenCalled());

    expect(order("Done").filter((title) => title.includes("Fresh"))).toHaveLength(1);
  });

  it("applies a create immediately when this user has nothing pending", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    live.frame(event("card.created", { card: cardBody("card-new", "c-done", "Fresh") }));

    await waitFor(() => expect(order("Done")).toEqual(["Fresh"]));
  });
});

describe("telling the user whether the board is live", () => {
  it("says so while connecting, once subscribed, and when it gives up", async () => {
    // #53's complaint: a board whose fan-out is gone renders exactly like a
    // correct one. The failure mode of a realtime feature is silence, and
    // silence is indistinguishable from nobody else editing.
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();

    expect(screen.getByText("Connecting…")).toBeInTheDocument();

    await goLive(live);

    live.close(4003, "membership revoked");

    await screen.findByText("Not live");
    expect(screen.getByText(/no longer have access/i)).toBeInTheDocument();
  });

  it("stops trying after a 4003 rather than retrying against a refusal", async () => {
    const live = fakeRealtime(respond(200));

    vi.stubGlobal("fetch", live.fetch);
    renderBoard();
    await goLive(live);

    live.close(4003, "membership revoked");
    await screen.findByText("Not live");

    const opened = live.opened();

    await new Promise((resolve) => setTimeout(resolve, 250));

    expect(live.opened()).toBe(opened);
  });
});
