/**
 * Rendering the API's timestamps.
 *
 * `crud.go`'s `timestamp` emits RFC 3339 in UTC, and `lib/api/types.ts` keeps
 * those as strings rather than parsing them into `Date` — a `Date` in an RSC
 * payload is a serialization concern nobody needs. So the parsing happens here,
 * once, at the point of display.
 *
 * # Why the format is fixed rather than the reader's locale
 *
 * These strings are produced during **server** rendering. `toLocaleDateString()`
 * with no locale would use the server's, which is whatever the container's
 * `LANG` happens to be — so the same page would read differently depending on
 * which task served it, and a change to a base image would silently reformat
 * every date in the app. Formatting on the client instead would swap that for a
 * hydration mismatch, because the server's first paint would not match the
 * browser's re-render.
 *
 * One explicit locale and one explicit time zone makes the output a property of
 * this function. Real per-reader localisation needs the reader's time zone,
 * which the server does not have; that is a feature, not a formatting tweak, and
 * it is not this issue.
 */

const DATE_FORMAT = new Intl.DateTimeFormat("en-GB", {
  day: "numeric",
  month: "short",
  year: "numeric",
  timeZone: "UTC",
});

/**
 * The same fixed locale and zone, to the minute.
 *
 * A card's "created" and "updated" are frequently the same day and often
 * minutes apart, so a date alone reports them as identical and tells the reader
 * nothing — which is the whole reason for showing both.
 *
 * The zone is spelled out in the output rather than left implicit. "14:32" with
 * no qualifier reads as the reader's own clock, and this one is not: it is UTC,
 * because the server has no way to know the reader's zone (see the note above
 * on why that is a feature and not a formatting tweak).
 */
const DATE_TIME_FORMAT = new Intl.DateTimeFormat("en-GB", {
  day: "numeric",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  timeZone: "UTC",
  timeZoneName: "short",
});

/**
 * A timestamp as a short date, or null when it was not one.
 *
 * Null rather than a fallback string: the caller decides whether an
 * unrenderable date is worth a line at all, and every caller here simply omits
 * it. An `Invalid Date` in the UI is worse than no date.
 */
export function formatDate(value: string | null | undefined): string | null {
  return format(value, DATE_FORMAT);
}

/**
 * A timestamp as a date and a time, or null when it was not one.
 *
 * Same contract as {@link formatDate} — the caller decides what an unrenderable
 * timestamp is worth — and used where two timestamps are shown side by side and
 * the difference between them is the information.
 */
export function formatDateTime(value: string | null | undefined): string | null {
  return format(value, DATE_TIME_FORMAT);
}

function format(
  value: string | null | undefined,
  formatter: Intl.DateTimeFormat,
): string | null {
  if (typeof value !== "string" || value === "") {
    return null;
  }

  const parsed = new Date(value);

  if (Number.isNaN(parsed.getTime())) {
    return null;
  }

  return formatter.format(parsed);
}
