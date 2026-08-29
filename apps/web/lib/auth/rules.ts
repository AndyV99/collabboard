/**
 * The client-side mirror of what `apps/api` actually enforces.
 *
 * # The rule about rules
 *
 * Every constant and every check below exists in `apps/api/internal/auth`
 * (`validateRegistration` and the constants above it, plus the `CHECK`
 * constraints in `migrations/00002_tenancy.sql`). Nothing here is stricter than
 * the API, and that is deliberate: a client-side rule the server would have
 * accepted is a rejection with no authority behind it. It tells the user their
 * password is bad when it is fine, and it cannot be relaxed without a deploy of
 * the wrong service.
 *
 * The reverse — being *looser* than the API — is fine. The server is the
 * authority; these checks exist to save a round trip and to put the message next
 * to the field it is about, not to be the gate.
 *
 * # Lengths are counted the way the API counts them
 *
 * Go's `len(password)` is a **byte** count, and `utf8.RuneCountInString` on the
 * display name is a **code point** count. JavaScript's `String.length` is
 * neither — it counts UTF-16 code units. For an ASCII password the three agree;
 * for "🔐🔐🔐🔐🔐🔐" they do not, and a naive `.length >= 12` would reject a
 * 24-byte password the API is happy with. So {@link byteLength} and
 * {@link codePointLength} do the counting, and each is named after the Go
 * function it mirrors.
 *
 * # Login is not validated against these
 *
 * {@link validateLogin} checks presence and nothing else. An account created
 * before a rule changed still has to be able to sign in, and "your password is
 * too short" on a sign-in form is both useless and a small statement about a
 * stored value. The only reason to check anything client-side on login is to
 * keep an empty form from spending one of the API's five per-account attempts.
 */

/** `MinPasswordLength` in `apps/api/internal/auth/service.go`. Bytes. */
export const MIN_PASSWORD_BYTES = 12;

/** `MaxPasswordLength`. Bytes. Bounds the argon2id input, not the entropy. */
export const MAX_PASSWORD_BYTES = 128;

/** `maxEmailLength`. Bytes; RFC 5321 puts the path at 254 octets. */
export const MAX_EMAIL_BYTES = 254;

/** `maxDisplayNameLength`. Code points, matching `utf8.RuneCountInString`. */
export const MAX_DISPLAY_NAME_CODE_POINTS = 128;

/**
 * `maxOrganizationNameLength` in `apps/api/internal/auth/service.go`. Code
 * points, matching `utf8.RuneCountInString`.
 *
 * A mirror since #67. It used to be the one number in this file that was not
 * one — `apps/api` checked no length at all, `organizations.name` was `text`
 * with only a "not blank" constraint, and this was a client-side mitigation set
 * to the display name's limit for want of anything better to copy.
 *
 * The service now caps it at 200, the same as every other name on the API
 * (`maxNameLength` in `internal/api/crud.go`), and `organizations` carries a
 * matching CHECK from migration 00007. Raising this from 128 to 200 is not
 * loosening a rule: at 128 this file was **stricter than the service**, which is
 * the one thing its opening paragraph says a rule here must never be.
 */
export const MAX_ORGANIZATION_NAME_CODE_POINTS = 200;

/** `len(s)` in Go: UTF-8 bytes. */
export function byteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

/** `utf8.RuneCountInString(s)` in Go: code points, not UTF-16 units. */
export function codePointLength(value: string): number {
  return [...value].length;
}

/** `auth.NormalizeEmail`: trimmed and lower-cased, exactly. */
export function normalizeEmail(email: string): string {
  return email.trim().toLowerCase();
}

/** The fields each form collects. */
export type LoginFields = {
  email: string;
  password: string;
};

export type RegistrationFields = LoginFields & {
  displayName: string;
  organizationName: string;
};

/** Field names, so an error map cannot name a field that does not exist. */
export type LoginField = keyof LoginFields;
export type RegistrationField = keyof RegistrationFields;

export type FieldErrors<F extends string> = Partial<Record<F, string>>;

/**
 * The order fields are checked in, which is the order they appear on screen.
 *
 * Focus goes to the first field with an error, so this list decides which one —
 * and "the first one that is wrong, reading down the form" is the only ordering
 * that matches what a sighted user sees and what a screen reader announces.
 */
export const LOGIN_FIELD_ORDER: readonly LoginField[] = ["email", "password"];

export const REGISTRATION_FIELD_ORDER: readonly RegistrationField[] = [
  "email",
  "password",
  "displayName",
  "organizationName",
];

/**
 * Presence only. See the module comment for why this does not check length.
 */
export function validateLogin(fields: LoginFields): FieldErrors<LoginField> {
  const errors: FieldErrors<LoginField> = {};

  if (normalizeEmail(fields.email) === "") {
    errors.email = "Enter your email address.";
  }

  if (fields.password === "") {
    errors.password = "Enter your password.";
  }

  return errors;
}

/**
 * Mirrors `validateRegistration` in `apps/api/internal/auth/service.go`, plus
 * the two database `CHECK` constraints it does not cover.
 *
 * The email check is "at least one character, then an `@`". Go only asks for an
 * `@` anywhere, but `users.email` carries
 * `CHECK (position('@' IN email) > 1)`, so `@example.com` passes the service and
 * fails the insert — a 500 rather than a 400. Refusing it here is not a stricter
 * rule than the API's; it is the API's own constraint, checked at the only point
 * where the user can still fix it. Filed upstream as #76: the service should
 * reject it with the other input errors.
 */
export function validateRegistration(
  fields: RegistrationFields,
): FieldErrors<RegistrationField> {
  const errors: FieldErrors<RegistrationField> = {};

  const email = normalizeEmail(fields.email);
  const displayName = fields.displayName.trim();
  const organizationName = fields.organizationName.trim();

  if (email === "") {
    errors.email = "Enter your email address.";
  } else if (email.indexOf("@") < 1) {
    errors.email = "Enter an email address, including the part before the @.";
  } else if (byteLength(email) > MAX_EMAIL_BYTES) {
    errors.email = `Email addresses can be at most ${MAX_EMAIL_BYTES} characters.`;
  }

  if (displayName === "") {
    errors.displayName = "Enter the name you want to be shown as.";
  } else if (codePointLength(displayName) > MAX_DISPLAY_NAME_CODE_POINTS) {
    errors.displayName = `Display names can be at most ${MAX_DISPLAY_NAME_CODE_POINTS} characters.`;
  }

  const passwordBytes = byteLength(fields.password);

  if (passwordBytes < MIN_PASSWORD_BYTES) {
    errors.password = `Use at least ${MIN_PASSWORD_BYTES} characters. Length is what makes a password strong — there are no other requirements.`;
  } else if (passwordBytes > MAX_PASSWORD_BYTES) {
    errors.password = `Passwords can be at most ${MAX_PASSWORD_BYTES} characters.`;
  }

  if (codePointLength(organizationName) > MAX_ORGANIZATION_NAME_CODE_POINTS) {
    errors.organizationName = `Workspace names can be at most ${MAX_ORGANIZATION_NAME_CODE_POINTS} characters.`;
  }

  return errors;
}

/** Whether a validation pass found anything. */
export function hasErrors<F extends string>(errors: FieldErrors<F>): boolean {
  return Object.keys(errors).length > 0;
}

/** The first field with an error, in screen order, or null. */
export function firstInvalidField<F extends string>(
  errors: FieldErrors<F>,
  order: readonly F[],
): F | null {
  return order.find((field) => errors[field] !== undefined) ?? null;
}

/**
 * What the API will call the workspace when the name is left blank.
 *
 * `Register` in `apps/api` does exactly this: an empty `organization_name`
 * becomes `displayName + "'s workspace"`. Shown as a hint under the field so
 * "optional" says what optional means, rather than leaving the user to find out
 * after they have committed to it.
 */
export function defaultOrganizationName(displayName: string): string {
  const trimmed = displayName.trim();

  return trimmed === "" ? "" : `${trimmed}'s workspace`;
}
