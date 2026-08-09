/**
 * The URLs, and the date rendering that goes next to them.
 */

import { describe, expect, it } from "vitest";

import { formatDate, formatDateTime } from "@/lib/workspace/format";
import {
  MEMBERS_PATH,
  WORKSPACE_PATH,
  boardHref,
  cardHref,
  projectHref,
  selectedCardId,
} from "@/lib/workspace/routes";

describe("workspace routes", () => {
  it("builds a project URL under the workspace root", () => {
    expect(projectHref("p-1")).toBe("/app/projects/p-1");
  });

  it("nests a board under its project", () => {
    // The API addresses a board flatly; the URL does not, because the page has
    // a breadcrumb to draw and fetching the board to find its project would be
    // a second round trip for one line of text.
    expect(boardHref("p-1", "b-1")).toBe("/app/projects/p-1/boards/b-1");
  });

  it("encodes ids so a hostile one cannot change the shape of the URL", () => {
    expect(projectHref("../members")).toBe("/app/projects/..%2Fmembers");
    expect(boardHref("p/1", "b?x=1")).toBe("/app/projects/p%2F1/boards/b%3Fx%3D1");
  });

  it("opens a card on its board rather than at a URL of its own", () => {
    // A card is a detail of the board, and the board stays on screen behind it.
    // As a search parameter the selection is still addressable, reloadable and
    // shareable, and it costs no request — the board already has every card.
    expect(cardHref("p-1", "b-1", "c-1")).toBe(
      "/app/projects/p-1/boards/b-1?card=c-1#card",
    );
  });

  it("encodes a card id into the query rather than letting it add parameters", () => {
    expect(cardHref("p-1", "b-1", "c-1&card=c-2")).toBe(
      "/app/projects/p-1/boards/b-1?card=c-1%26card%3Dc-2#card",
    );
  });

  it("keeps every workspace URL under the protected group's path", () => {
    // Route protection is `app/(protected)/layout.tsx` and it covers a subtree.
    // A workspace URL that escaped /app would be a page with no session check.
    for (const href of [
      WORKSPACE_PATH,
      MEMBERS_PATH,
      projectHref("p-1"),
      boardHref("p-1", "b-1"),
      cardHref("p-1", "b-1", "c-1"),
    ]) {
      expect(href.startsWith("/app")).toBe(true);
    }
  });
});

describe("selectedCardId", () => {
  it("reads the open card out of the query", () => {
    expect(selectedCardId({ card: "c-1" })).toBe("c-1");
  });

  it("is null when no card is open", () => {
    expect(selectedCardId({})).toBeNull();
    expect(selectedCardId({ card: "" })).toBeNull();
  });

  it("refuses to pick one when the URL names two", () => {
    // `?card=a&card=b` is not a request this screen can honour, and obeying
    // half of it would leave the URL no longer describing what is on screen.
    expect(selectedCardId({ card: ["c-1", "c-2"] })).toBeNull();
  });
});

describe("formatDate", () => {
  it("renders an RFC 3339 timestamp the same way wherever it runs", () => {
    // Fixed locale and fixed time zone, because this runs during server
    // rendering: a locale-dependent format would differ between tasks and would
    // change under a base-image bump.
    expect(formatDate("2026-08-09T23:30:00Z")).toBe("9 Aug 2026");
  });

  it("uses UTC rather than the machine's zone", () => {
    // 23:30Z is already the 10th in Sydney and still the 9th in London. Without
    // an explicit zone the answer would depend on where the container runs.
    expect(formatDate("2026-08-09T23:59:59Z")).toBe("9 Aug 2026");
  });

  it("returns null rather than 'Invalid Date'", () => {
    expect(formatDate("not a date")).toBeNull();
    expect(formatDate("")).toBeNull();
    expect(formatDate(null)).toBeNull();
    expect(formatDate(undefined)).toBeNull();
  });
});

describe("formatDateTime", () => {
  it("names the zone, because the time is not the reader's", () => {
    // A card's created and updated are often minutes apart, so the card detail
    // needs the minute — and an unqualified "14:32" would read as local time.
    expect(formatDateTime("2026-08-09T14:32:00Z")).toBe("9 Aug 2026, 14:32 UTC");
  });

  it("returns null rather than 'Invalid Date'", () => {
    expect(formatDateTime("not a date")).toBeNull();
    expect(formatDateTime(null)).toBeNull();
  });
});
