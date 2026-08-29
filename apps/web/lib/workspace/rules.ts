/**
 * Client-side validation for the workspace forms, mirroring what `apps/api`
 * enforces and never exceeding it.
 *
 * # The rule about rules, restated
 *
 * `lib/auth/rules.ts` establishes it for the auth screens and the same rule
 * holds here: nothing below is stricter than the service, because a client-side
 * rejection the server would have accepted is a rejection with no authority
 * behind it — and it cannot be relaxed without deploying the wrong service.
 * Being *looser* is fine. These checks save a round trip and put the message
 * next to the field; the API is the gate.
 *
 * Every constant is a mirror of one in `apps/api/internal/api/crud.go`
 * (`maxNameLength`, `maxDescriptionLength`) or
 * `apps/api/internal/auth/service.go` (`maxEmailLength`), and every check is a
 * mirror of `requiredText`, `boundedText`, `optionalText` or
 * `validateAddMember`.
 *
 * # Counting, and trimming, the way Go does
 *
 * The API counts names and descriptions with `utf8.RuneCountInString` — code
 * points — and counts an address with `len()` — bytes. JavaScript's
 * `String.length` is neither, so {@link codePointLength} and {@link byteLength}
 * from `lib/auth/rules.ts` do the counting rather than being reimplemented here.
 *
 * `requiredText` trims *before* it checks and stores the trimmed value, so
 * `"   "` is a blank name rather than a name that renders as nothing. Every
 * check below trims first for the same reason, and the forms send the trimmed
 * value so that what was validated is what is stored.
 */

import { byteLength, codePointLength } from "@/lib/auth/rules";
import { sameDue } from "@/lib/board/due";

/** `maxNameLength` in `apps/api/internal/api/crud.go`. Code points. */
export const MAX_NAME_CODE_POINTS = 200;

/** `maxDescriptionLength`. Code points. */
export const MAX_DESCRIPTION_CODE_POINTS = 10_000;

/** `maxEmailLength` in `apps/api/internal/auth/service.go`. Bytes. */
export const MAX_EMAIL_BYTES = 254;

/**
 * A field problem, or `undefined` when the field is fine.
 *
 * One string rather than a code, because there is exactly one consumer — the
 * field it is about — and a code would need a lookup table that says the same
 * thing in another file.
 */
export type FieldError = string | undefined;

/**
 * A `maxLength` attribute that cannot be stricter than a code-point limit.
 *
 * `maxLength` counts **UTF-16 code units**; every limit in this file counts
 * **code points**, because the API counts runes with `utf8.RuneCountInString`.
 * The two agree for Latin text and disagree for anything outside the BMP: a
 * 200-emoji title is 200 code points and 400 code units, so `maxLength={200}`
 * would stop the browser at 100 emoji — refusing input `requiredText` accepts.
 *
 * A code point is at most two code units, so twice the limit is the smallest
 * value that can never be the stricter of the two. The attribute stays what
 * `components/workspace/fields.tsx` says it is — a courtesy stop against a
 * runaway paste — and {@link validateName} and friends remain the rule.
 *
 * The forms written before #64 pass the limit itself. Filed as its own issue
 * rather than corrected here, because it is a bug in three other screens.
 */
export function maxLengthFor(codePoints: number): number {
  return codePoints * 2;
}

/**
 * A name every create form and the rename form share.
 *
 * Mirrors `requiredText(c, "name", ..., maxNameLength)`. `subject` is the word
 * the message uses ("Project", "Board"), so one function covers both without
 * either message reading as though it were written for the other thing.
 */
export function validateName(value: string, subject: string): FieldError {
  const trimmed = value.trim();

  if (trimmed === "") {
    return `Give the ${subject.toLowerCase()} a name.`;
  }

  if (codePointLength(trimmed) > MAX_NAME_CODE_POINTS) {
    return `${subject} names can be at most ${MAX_NAME_CODE_POINTS} characters.`;
  }

  return undefined;
}

/**
 * The optional description on a project.
 *
 * Mirrors `boundedText`: empty is a value, not a failure. A project may have no
 * description, and the rename form may clear one — `optionalText(..., allowEmpty: true)`
 * on the API side says so explicitly.
 */
export function validateDescription(value: string): FieldError {
  if (codePointLength(value.trim()) > MAX_DESCRIPTION_CODE_POINTS) {
    return `Descriptions can be at most ${MAX_DESCRIPTION_CODE_POINTS} characters.`;
  }

  return undefined;
}

/**
 * A card's title.
 *
 * Mirrors `requiredText(c, "title", req.Title, maxNameLength)` in
 * `apps/api/internal/api/cards.go` — the *same* 200 code points a name gets,
 * because `maxNameLength` is one constant serving both. It is a separate
 * function from {@link validateName} only so the copy can say "title", which is
 * what the field is called on screen and in the API's own 400.
 *
 * `PATCH /cards/:id` is stricter than the create path in one way worth knowing:
 * `optionalText(..., allowEmpty: false)` means a title sent as `""` is a 400
 * ("title cannot be empty") rather than a no-op, so the same check has to run
 * before an edit as before a create.
 */
export function validateCardTitle(value: string): FieldError {
  const trimmed = value.trim();

  if (trimmed === "") {
    return "Give the card a title.";
  }

  if (codePointLength(trimmed) > MAX_NAME_CODE_POINTS) {
    return `Card titles can be at most ${MAX_NAME_CODE_POINTS} characters.`;
  }

  return undefined;
}

/**
 * A due date, as the card editor collects it.
 *
 * Mirrors `parseDueAt` in `apps/api/internal/api/cardfields.go`, which answers
 * 400 for anything `time.Parse(time.RFC3339, ...)` refuses. Blank is not a
 * failure: an empty control is "no due date", which `PATCH /cards/:id` accepts
 * as a null and treats as a request to clear the field.
 *
 * This is looser than the API in one direction and never stricter, which is the
 * rule this file exists to keep. `Date` accepts a few forms `time.RFC3339` does
 * not — a date with no time, most usefully — and `lib/board/due.ts` converts
 * whatever it accepted into an instant with an offset before it is sent, so the
 * request the API sees is always one it can parse.
 *
 * Reachable at all only because `<input type="datetime-local">` degrades to a
 * plain text box where it is unsupported. Where it is supported the browser is
 * already refusing everything below.
 */
export function validateDueAt(value: string): FieldError {
  const trimmed = value.trim();

  if (trimmed === "") {
    return undefined;
  }

  if (Number.isNaN(new Date(trimmed).getTime())) {
    return "Enter a date and time, or clear the field to remove the due date.";
  }

  return undefined;
}

/** The four fields of a card that `PATCH /cards/:id` can change. */
export type CardFields = {
  title: string;
  description: string;
  assigneeId: string | null;
  dueAt: string | null;
};

/**
 * Whether a card edit would say anything.
 *
 * `patchCardHandler` answers 400 for a body that mentions none of the four
 * fields ("at least one of title, description, assignee_id or due_at is
 * required"), so submitting an untouched card would collect an error for doing
 * nothing.
 *
 * The comparison is against the *trimmed* input because the API trims before it
 * stores: a title with a trailing space added is not a change the server would
 * record, so offering to save it would produce a write that changes nothing and
 * a board that re-renders identically.
 *
 * The due date is compared with {@link sameDue} rather than by string, because
 * the control that collects it has minute granularity and the stored value does
 * not — see its comment for why comparing instants here would send a PATCH
 * every time somebody opened a card and pressed Save.
 */
export function cardChanged(current: CardFields, next: CardFields): boolean {
  return (
    current.title !== next.title.trim() ||
    current.description !== next.description.trim() ||
    current.assigneeId !== next.assigneeId ||
    !sameDue(current.dueAt, next.dueAt)
  );
}

/**
 * The address on the add-member form.
 *
 * Mirrors `validateAddMember`, plus one check the service does not make: at
 * least one character before the `@`. That is not a stricter rule — it is
 * `users.email`'s own `CHECK (position('@' IN email) > 1)`, so an account with
 * such an address cannot exist and submitting one can only ever produce the 404
 * that says "no account with that email address". Refusing it here turns a
 * confusing answer into a clear one, and it is the same check
 * `validateRegistration` makes for the same constraint (see #76).
 */
export function validateMemberEmail(value: string): FieldError {
  const email = value.trim().toLowerCase();

  if (email === "") {
    return "Enter the email address of the person to add.";
  }

  if (email.indexOf("@") < 1) {
    return "Enter an email address, including the part before the @.";
  }

  if (byteLength(email) > MAX_EMAIL_BYTES) {
    return `Email addresses can be at most ${MAX_EMAIL_BYTES} characters.`;
  }

  return undefined;
}

/**
 * Whether a PATCH would say anything.
 *
 * `patchProjectHandler` answers 400 for a body that mentions neither field
 * ("at least one of name or description is required"), so a rename form that
 * submits an unchanged project would collect an error for doing nothing. This
 * is how the form knows to leave the button alone instead.
 */
export function projectChanged(
  current: { name: string; description: string },
  next: { name: string; description: string },
): boolean {
  return (
    current.name !== next.name.trim() || current.description !== next.description.trim()
  );
}
