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
