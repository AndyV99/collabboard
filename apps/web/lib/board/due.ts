/**
 * A card's due date: what it says, when it is late, and how it survives the
 * round trip through a form control.
 *
 * Every function here is pure. `now` is always an argument rather than a call
 * to `Date.now()` inside, which is what makes "this card is overdue" a table of
 * inputs and expected outputs instead of a test that has to move the clock.
 *
 * # Overdue is an instant comparison, so it has no time zone
 *
 * `due_at` is a `timestamptz` and reaches the browser as RFC 3339 in UTC. A
 * card due at 17:00Z is late at 17:00:01Z for a reader in Auckland and for one
 * in Los Angeles alike — they disagree about what *day* that is, never about
 * whether it has happened. So {@link isOverdue} compares two instants and is
 * correct in every zone by construction; nothing about it needs the reader's.
 *
 * # What *is* zone-dependent is the date on the card, and that is decided late
 *
 * A deadline is a wall-clock commitment: "the 31st" means the reader's 31st,
 * and rendering `2026-09-01T00:30:00Z` as "1 Sep" to somebody in UTC+10 tells
 * them a card is due tomorrow when it is due this afternoon. That is a worse
 * failure than it looks, because it is invisible — the date shown is a real
 * date and nothing about it says it belongs to another clock.
 *
 * But the reader's zone is not knowable on the server, and `lib/workspace/format.ts`
 * spells out why guessing it there is not a formatting tweak: the container's
 * `LANG` and `TZ` are not the reader's, and a client-side re-render that
 * disagrees with the server's first paint is a hydration mismatch.
 *
 * {@link dueLabel} resolves that by taking `now` as `number | null`, where null
 * means "the reader's clock is not known yet" — which is true during server
 * rendering *and* during the first client render, so the two produce identical
 * HTML and React has nothing to complain about. Null renders the same fixed-UTC
 * form every other timestamp in this app uses, with the zone named so it is not
 * a lie. Once `components/boards/use-due-clock.ts` reports a clock, the label
 * is re-rendered in the reader's own zone.
 *
 * The cost is one re-render's worth of flicker for readers outside UTC. The
 * alternative — freezing every due date in the server's zone — is wrong for
 * most of the world and says nothing about being wrong.
 */

import { formatDateTime } from "@/lib/workspace/format";

/**
 * The reader's own zone and locale-independent formatting.
 *
 * No `timeZone` option, so this resolves to the runtime's — which in a browser
 * is the reader's. It is only ever *called* with a non-null clock, i.e. only
 * after mount, so the server's zone never reaches a rendered string through it.
 *
 * The locale stays fixed for the same reason `lib/workspace/format.ts` fixes
 * it: `en-GB` is a decision about what the output looks like, and the reader's
 * `Accept-Language` is a separate feature from the reader's clock.
 */
const READER_FORMAT = new Intl.DateTimeFormat("en-GB", {
  day: "numeric",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
});

/** Whether a due instant has already passed. False for anything unparseable. */
export function isOverdue(dueAt: string | null, now: number): boolean {
  if (dueAt === null) {
    return false;
  }

  const at = Date.parse(dueAt);

  return !Number.isNaN(at) && at < now;
}

/**
 * A due date as the reader should read it, or null when it is not a timestamp.
 *
 * Null `now` is "before the reader's clock is known" — see the module comment.
 * Null return is the same contract `formatDate` and `formatDateTime` have: the
 * caller decides what an unrenderable timestamp is worth, and every caller here
 * omits it rather than printing `Invalid Date`.
 */
export function dueLabel(dueAt: string | null, now: number | null): string | null {
  if (dueAt === null) {
    return null;
  }

  if (now === null) {
    return formatDateTime(dueAt);
  }

  const at = new Date(dueAt);

  return Number.isNaN(at.getTime()) ? null : READER_FORMAT.format(at);
}

/**
 * A due instant as `<input type="datetime-local">` wants it: local wall clock,
 * `YYYY-MM-DDTHH:mm`, no offset.
 *
 * The local getters are the whole point. The control has no notion of a zone —
 * it shows and returns the reader's wall clock — so an instant has to be
 * converted into *their* clock on the way in and back out of it on the way out
 * ({@link dueFromInput}). Slicing `toISOString()` instead would put UTC's clock
 * into the box, and a reader in UTC+2 would see a card due at 17:00 offered for
 * editing as 15:00 and "fix" it by two hours.
 *
 * Returns "" for null or for a value that is not a timestamp, which is the
 * empty control — an unset due date and an unrenderable one both leave the
 * reader with a blank box they can fill in.
 */
export function dueToInput(dueAt: string | null): string {
  if (dueAt === null) {
    return "";
  }

  const at = new Date(dueAt);

  if (Number.isNaN(at.getTime())) {
    return "";
  }

  const year = String(at.getFullYear()).padStart(4, "0");
  const month = pad(at.getMonth() + 1);
  const day = pad(at.getDate());

  return `${year}-${month}-${day}T${pad(at.getHours())}:${pad(at.getMinutes())}`;
}

/**
 * The instant a `datetime-local` value names, as the API's RFC 3339 with an
 * offset — or null when the control is empty, which is "no due date".
 *
 * `new Date("2026-01-31T17:00")` is **local** by specification: a date-time
 * string without an offset is interpreted against the runtime's zone, where a
 * date-only string is interpreted as UTC. That asymmetry is why this only ever
 * sees the control's full `YYYY-MM-DDTHH:mm` form, and why
 * {@link validateDueAt} in `lib/workspace/rules.ts` refuses anything else
 * before it gets here.
 *
 * `toISOString()` gives `…Z`, which is an offset — `parseDueAt` on the API side
 * requires one, because a `timestamptz` has no local clock to fall back on.
 */
export function dueFromInput(value: string): string | null {
  const trimmed = value.trim();

  if (trimmed === "") {
    return null;
  }

  const at = new Date(trimmed);

  return Number.isNaN(at.getTime()) ? null : at.toISOString();
}

/**
 * Whether two due dates are the same *as the form can express them*.
 *
 * Not an instant comparison, deliberately. `datetime-local` has minute
 * granularity, so a card due at `17:00:30Z` is shown in the box as 17:00 and
 * read back out as `17:00:00Z` — thirty seconds earlier. Comparing instants
 * would call that a change and send a PATCH nobody asked for every time
 * somebody opened a card and pressed Save; comparing what the control shows
 * calls it what it is, which is the same minute.
 *
 * When the reader *does* edit the date those seconds are dropped for real,
 * which is the value they picked.
 */
export function sameDue(a: string | null, b: string | null): boolean {
  return dueToInput(a) === dueToInput(b);
}

function pad(value: number): string {
  return String(value).padStart(2, "0");
}
