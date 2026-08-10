/**
 * The board as rendered: columns across, cards down, one card open.
 *
 * `BoardView` and `CardDetail` take resolved props — the page fetches, these
 * render — so every state below is a render rather than something that has to
 * be provoked against a real API. That split is what let #64 and #65 add
 * `"use client"` to these files without moving the data flow.
 *
 * # Why this file now mocks the router
 *
 * It did not have to before #65, and that was worth something: #64's controls
 * all mount on demand, so a board nobody was editing called `useRouter` nowhere
 * and this file could render it with no app-router context at all.
 *
 * A card is draggable without anyone opening anything, so the runner that sends
 * a move is mounted for every board, and `useRouter` with it. The mock is the
 * honest consequence of the feature rather than a test bent to fit — but the
 * property it replaces is a real loss, and it is the reason this comment exists
 * instead of a bare `vi.mock`.
 */

import { render, screen, within } from "@testing-library/react";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ push: vi.fn(), refresh: vi.fn() }),
}));

import type { Card, Column } from "@/lib/api/types";

vi.mock("next/link", () => ({
  default: ({ href, children, ...rest }: { href: string; children: ReactNode }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

const { BoardView } = await import("@/components/boards/board-view");
const { CardDetail, CardNotOnBoard } = await import("@/components/boards/card-detail");
const { BoardSkeleton } = await import("@/components/boards/board-skeleton");
const { groupCardsIntoColumns } = await import("@/lib/board/snapshot");

const BOARD = "b-1";

function column(id: string, name: string): Column {
  return {
    id,
    boardId: BOARD,
    name,
    createdAt: "2026-08-01T09:00:00Z",
    updatedAt: "2026-08-01T09:00:00Z",
  };
}

function card(id: string, columnId: string, title: string, description = ""): Card {
  return {
    id,
    boardId: BOARD,
    columnId,
    title,
    description,
    createdAt: "2026-08-02T09:00:00Z",
    updatedAt: "2026-08-02T14:32:00Z",
  };
}

const COLUMNS = [column("c-todo", "To do"), column("c-doing", "Doing")];

// Deliberately in a sequence a move produced: not creation order, not
// alphabetical. See __tests__/board-snapshot.test.ts.
const CARDS = [
  card("card-3", "c-doing", "Zebra"),
  card("card-2", "c-doing", "Kilo"),
  card("card-1", "c-doing", "Alpha"),
  card("card-4", "c-todo", "Mike", "Something to do"),
];

function renderBoard(
  columns: Column[] = COLUMNS,
  cards: Card[] = CARDS,
  selectedCardId: string | null = null,
) {
  return render(
    <BoardView
      boardId={BOARD}
      projectId="p-1"
      selectedCardId={selectedCardId}
      snapshot={groupCardsIntoColumns(columns, cards)}
    />,
  );
}

describe("BoardView", () => {
  it("renders the columns in the API's order", () => {
    renderBoard();

    expect(screen.getAllByRole("heading", { level: 3 }).map((h) => h.textContent)).toEqual(
      ["To do", "Doing"],
    );
  });

  it("puts each card under its own column", () => {
    renderBoard();

    const doing = screen.getByRole("list", { name: "Doing" });

    expect(within(doing).getAllByRole("link").map((a) => a.textContent)).toEqual([
      "Zebra",
      "Kilo",
      "Alpha",
    ]);
  });

  it("renders cards in the order the API returned them, not a sorted one", () => {
    renderBoard();

    const doing = screen.getByRole("list", { name: "Doing" });
    const titles = within(doing)
      .getAllByRole("link")
      .map((a) => a.textContent ?? "");

    expect(titles).not.toEqual([...titles].sort());
    expect(titles).toEqual(["Zebra", "Kilo", "Alpha"]);
  });

  it("links each card to the board's own URL with the card open", () => {
    renderBoard();

    expect(screen.getByRole("link", { name: /Zebra/ })).toHaveAttribute(
      "href",
      "/app/projects/p-1/boards/b-1?card=card-3#card",
    );
  });

  it("marks the open card as current", () => {
    renderBoard(COLUMNS, CARDS, "card-2");

    expect(screen.getByRole("link", { name: /Kilo/ })).toHaveAttribute(
      "aria-current",
      "true",
    );
    expect(screen.getByRole("link", { name: /Zebra/ })).not.toHaveAttribute(
      "aria-current",
    );
  });

  it("counts the cards in each column", () => {
    renderBoard();

    expect(screen.getByText("3 cards")).toBeInTheDocument();
    expect(screen.getByText("1 card")).toBeInTheDocument();
  });

  it("says a column is empty rather than rendering an empty list", () => {
    renderBoard(COLUMNS, [CARDS[3]]);

    expect(screen.getByText("No cards in this column.")).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Doing" })).not.toBeInTheDocument();
  });

  it("shows a card's description on the tile only when it has one", () => {
    renderBoard();

    expect(screen.getByText("Something to do")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Zebra" })).toBeInTheDocument();
  });

  it("makes each column's card list keyboard-scrollable", () => {
    // A scroll container with no focusable ancestor is unreachable without a
    // pointer, which is every column whose cards run past the fold.
    renderBoard();

    expect(screen.getByRole("list", { name: "Doing" })).toHaveAttribute("tabindex", "0");
  });
});

describe("CardDetail", () => {
  const OPEN = card("card-2", "c-doing", "Kilo", "First line\nSecond line");

  it("shows the title, the description and the column it is in", () => {
    render(<CardDetail card={OPEN} closeHref="/board" columnName="Doing" />);

    expect(screen.getByRole("heading", { name: "Kilo" })).toBeInTheDocument();
    expect(screen.getByText(/First line/)).toBeInTheDocument();
    expect(screen.getByText("Doing")).toBeInTheDocument();
  });

  it("shows both timestamps to the minute, so they can differ visibly", () => {
    render(<CardDetail card={OPEN} closeHref="/board" columnName="Doing" />);

    expect(screen.getByText(/2 Aug 2026, 09:00 UTC/)).toBeInTheDocument();
    expect(screen.getByText(/2 Aug 2026, 14:32 UTC/)).toBeInTheDocument();
  });

  it("never shows a position or a rank", () => {
    // ADR 0004: no response contains one, so nothing here can render one.
    const { container } = render(
      <CardDetail card={OPEN} closeHref="/board" columnName="Doing" />,
    );

    expect(container.textContent).not.toMatch(/position|rank/i);
  });

  it("says so when the card has no description", () => {
    render(
      <CardDetail card={card("card-1", "c-doing", "Alpha")} closeHref="/board" columnName="Doing" />,
    );

    expect(screen.getByText("This card has no description.")).toBeInTheDocument();
  });

  it("says so when the card's column is not on the board", () => {
    render(<CardDetail card={OPEN} closeHref="/board" columnName={null} />);

    expect(
      screen.getByText("Not in a column shown on this board"),
    ).toBeInTheDocument();
  });

  it("closes back to the board", () => {
    render(<CardDetail card={OPEN} closeHref="/app/projects/p-1/boards/b-1" columnName="Doing" />);

    expect(screen.getByRole("link", { name: "Close" })).toHaveAttribute(
      "href",
      "/app/projects/p-1/boards/b-1",
    );
  });
});

describe("CardNotOnBoard", () => {
  it("explains a ?card= that names nothing here, without claiming to know why", () => {
    render(<CardNotOnBoard closeHref="/app/projects/p-1/boards/b-1" />);

    expect(screen.getByRole("heading", { name: "Card not found" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Close" })).toHaveAttribute(
      "href",
      "/app/projects/p-1/boards/b-1",
    );
  });
});

describe("BoardSkeleton", () => {
  it("announces one status line and hides the placeholder boxes", () => {
    const { container } = render(<BoardSkeleton />);

    expect(screen.getByRole("status")).toHaveTextContent("Loading this board…");
    expect(container.querySelector("[aria-hidden='true']")).not.toBeNull();
  });
});
