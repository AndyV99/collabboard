/**
 * The URLs, and the date rendering that goes next to them.
 */

import { describe, expect, it } from "vitest";

import { formatDate } from "@/lib/workspace/format";
import {
  MEMBERS_PATH,
  WORKSPACE_PATH,
  boardHref,
  projectHref,
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

  it("keeps every workspace URL under the protected group's path", () => {
    // Route protection is `app/(protected)/layout.tsx` and it covers a subtree.
    // A workspace URL that escaped /app would be a page with no session check.
    for (const href of [
      WORKSPACE_PATH,
      MEMBERS_PATH,
      projectHref("p-1"),
      boardHref("p-1", "b-1"),
    ]) {
      expect(href.startsWith("/app")).toBe(true);
    }
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
