/**
 * A card's assignee and due date, as arithmetic.
 *
 * Everything under test here is pure, and every clock is an argument, so these
 * are a table of inputs and expected outputs rather than a board that has to be
 * rendered and a system time that has to be moved.
 *
 * # The zone is the point
 *
 * `vitest.config.mts` runs this suite in `Pacific/Auckland` on purpose. Half of
 * what is asserted below is only meaningful when the runtime's zone is not UTC:
 * a due date at 23:00 on 31 August is the **1st of September** to this reader,
 * and code that renders "31 Aug" is wrong in a way no UTC machine can detect.
 *
 * The offset used throughout is +12 (NZST). New Zealand's DST begins on the
 * last Sunday in September, so every August and early-September instant below
 * is unambiguously twelve hours ahead of UTC.
 */

import { describe, expect, it } from "vitest";

import { assigneeName, initialsFor } from "@/lib/board/assignee";
import {
  dueFromInput,
  dueLabel,
  dueToInput,
  isOverdue,
  sameDue,
} from "@/lib/board/due";
import type { Member } from "@/lib/api/types";

/** 23:00 UTC on the 31st, which is 11:00 on the 1st where this suite runs. */
const LATE_AUGUST = "2026-08-31T23:00:00Z";

function member(userId: string, displayName: string): Member {
  return {
    membershipId: `mem-${userId}`,
    userId,
    email: `${userId}@example.test`,
    displayName,
    role: "member",
    joinedAt: "2026-07-01T09:00:00Z",
  };
}

describe("isOverdue", () => {
  const due = Date.parse("2026-08-31T17:00:00Z");

  it("is true once the instant has passed and false before it", () => {
    expect(isOverdue("2026-08-31T17:00:00Z", due + 1)).toBe(true);
    expect(isOverdue("2026-08-31T17:00:00Z", due - 1)).toBe(false);
  });

  it("is not overdue at the instant itself", () => {
    // A card due at 17:00 is due *at* 17:00, not late at it. One millisecond
    // either side is the whole of the boundary, and picking the wrong side of
    // it is the only mistake this function can make.
    expect(isOverdue("2026-08-31T17:00:00Z", due)).toBe(false);
  });

  it("says nothing about a card with no due date", () => {
    expect(isOverdue(null, due + 1_000_000)).toBe(false);
  });

  it("does not call an unparseable value overdue", () => {
    // `Date.parse` gives NaN, and every comparison with NaN is false. Asserted
    // rather than assumed, because the alternative — a card the board insists
    // is late and no date beside it — is unexplainable to whoever sees it.
    expect(isOverdue("not a timestamp", due + 1_000_000)).toBe(false);
  });

  /*
   * The zone-independence claim, made concrete.
   *
   * An instant is an instant: the same `due_at` and the same `now` produce the
   * same answer regardless of where either is read, which is why `isOverdue`
   * never asks for a zone. The two clocks below are the same moment expressed
   * from either side of the date line.
   */
  it("is the same answer whichever zone the reader is in", () => {
    const nowInUtcTerms = Date.parse("2026-09-01T09:00:00Z");
    const nowInLocalTerms = Date.parse("2026-09-01T21:00:00+12:00");

    expect(nowInLocalTerms).toBe(nowInUtcTerms);
    expect(isOverdue(LATE_AUGUST, nowInUtcTerms)).toBe(true);
    expect(isOverdue(LATE_AUGUST, nowInLocalTerms)).toBe(true);
  });
});

describe("dueLabel", () => {
  it("renders the fixed UTC form before the reader's clock is known", () => {
    // What the server renders, and what the browser's first render has to match
    // it with. The zone is named, so a reader outside UTC is told which clock
    // they are looking at rather than left to assume it is theirs.
    expect(dueLabel(LATE_AUGUST, null)).toBe("31 Aug 2026, 23:00 UTC");
  });

  it("renders the reader's own day once there is a clock", () => {
    // The same instant, and a different date. This is the failure the null-clock
    // form would freeze in place if it were the only form: a card due this
    // afternoon shown as due yesterday.
    expect(dueLabel(LATE_AUGUST, Date.now())).toBe("1 Sept 2026, 11:00");
  });

  it("has nothing to say about a card with no due date", () => {
    expect(dueLabel(null, null)).toBeNull();
    expect(dueLabel(null, Date.now())).toBeNull();
  });

  it("returns null rather than 'Invalid Date' for a value that is not one", () => {
    expect(dueLabel("nonsense", null)).toBeNull();
    expect(dueLabel("nonsense", Date.now())).toBeNull();
  });
});

describe("dueToInput and dueFromInput", () => {
  it("puts the reader's wall clock into the control, not UTC's", () => {
    // `<input type="datetime-local">` has no zone. Slicing the ISO string would
    // offer this card for editing at 23:00 on the 31st, and a reader who saved
    // it unchanged would move the deadline twelve hours.
    expect(dueToInput(LATE_AUGUST)).toBe("2026-09-01T11:00");
  });

  it("reads the control back as the instant it names", () => {
    expect(dueFromInput("2026-09-01T11:00")).toBe(LATE_AUGUST.replace("Z", ".000Z"));
  });

  it("round-trips an instant through the control unchanged", () => {
    // The property the two functions have to have together, stated once rather
    // than left implied by the two directions being asserted separately.
    expect(dueFromInput(dueToInput(LATE_AUGUST))).toBe(
      new Date(LATE_AUGUST).toISOString(),
    );
  });

  it("sends an offset, because the API requires one", () => {
    // `parseDueAt` uses `time.Parse(time.RFC3339, ...)`, which refuses a
    // timestamp with no offset — a `timestamptz` has no local clock to fall
    // back on. `toISOString` always ends in Z.
    expect(dueFromInput("2026-09-01T11:00")?.endsWith("Z")).toBe(true);
  });

  it("treats an empty control as no due date rather than as an error", () => {
    expect(dueFromInput("")).toBeNull();
    expect(dueFromInput("   ")).toBeNull();
    expect(dueToInput(null)).toBe("");
  });

  it("offers an empty control for a value it cannot render", () => {
    expect(dueToInput("nonsense")).toBe("");
  });

  it("pads every component, so the control accepts what it is given", () => {
    // A single-digit month or hour is not a valid `datetime-local` value and
    // the control silently shows empty, which reads as "this card has no due
    // date" — the same bug as dropping the field.
    expect(dueToInput("2026-01-02T21:05:00Z")).toBe("2026-01-03T10:05");
  });
});

describe("sameDue", () => {
  it("ignores the seconds the control cannot show", () => {
    // Otherwise opening a card due at 17:00:30 and pressing Save without
    // touching anything would send a PATCH moving it thirty seconds earlier.
    expect(sameDue("2026-08-31T17:00:30Z", "2026-08-31T17:00:00Z")).toBe(true);
  });

  it("still sees a change of a minute", () => {
    expect(sameDue("2026-08-31T17:01:00Z", "2026-08-31T17:00:00Z")).toBe(false);
  });

  it("sees setting and clearing a due date", () => {
    expect(sameDue(null, "2026-08-31T17:00:00Z")).toBe(false);
    expect(sameDue("2026-08-31T17:00:00Z", null)).toBe(false);
    expect(sameDue(null, null)).toBe(true);
  });

  it("treats the same instant written two ways as one date", () => {
    expect(sameDue("2026-08-31T17:00:00Z", "2026-09-01T05:00:00+12:00")).toBe(true);
  });
});

describe("assigneeName", () => {
  const members = [member("user-dana", "Dana Okoro"), member("user-sam", "Sam Ito")];

  it("finds the member a card names", () => {
    expect(assigneeName(members, "user-sam")).toBe("Sam Ito");
  });

  it("is null for an id the list does not contain", () => {
    // A member added since this page was read. The screen says "assigned to
    // somebody not in this list" rather than printing the uuid.
    expect(assigneeName(members, "user-new")).toBeNull();
  });

  it("is null when the member list did not load at all", () => {
    expect(assigneeName(null, "user-dana")).toBeNull();
  });
});

describe("initialsFor", () => {
  it("takes the first letter of the first and last words", () => {
    expect(initialsFor("Dana Okoro")).toBe("DO");
    expect(initialsFor("Ada Byron King Lovelace")).toBe("AL");
  });

  it("takes one letter from a single-word name", () => {
    expect(initialsFor("Prince")).toBe("P");
  });

  it("counts by code point, not by UTF-16 unit", () => {
    // `"𝒜da"[0]` is half a surrogate pair and renders as a replacement
    // character. The same counting mistake `maxLengthFor` exists to avoid.
    expect(initialsFor("𝒜da Lovelace")).toBe("𝒜L");
  });

  it("survives a name that is only whitespace", () => {
    expect(initialsFor("   ")).toBe("?");
  });
});
