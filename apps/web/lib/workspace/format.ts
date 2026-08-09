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
 * A timestamp as a short date, or null when it was not one.
 *
 * Null rather than a fallback string: the caller decides whether an
 * unrenderable date is worth a line at all, and every caller here simply omits
 * it. An `Invalid Date` in the UI is worse than no date.
 */
export function formatDate(value: string | null | undefined): string | null {
  if (typeof value !== "string" || value === "") {
    return null;
  }

  const parsed = new Date(value);

  if (Number.isNaN(parsed.getTime())) {
    return null;
  }

  return DATE_FORMAT.format(parsed);
}
