/**
 * Turning a response from `app/api/auth/*` into something to put on the screen.
 *
 * Pure functions over `(status, message, headers)` so every branch of the copy
 * is a unit test rather than a manual reproduction of a 429.
 *
 * # The copy is part of the security model
 *
 * `apps/api` answers an unknown address and a wrong password identically — same
 * status, same message, and the same one argon2id derivation either way, so the
 * timings match too (see `Service.Login` and `absentAccountParams`). Issue #35
 * went to real trouble for that. A screen that says "we don't have an account
 * for that address" gives it all back, and so, more quietly, does a screen that
 * only offers "create an account instead?" when the address is unknown.
 *
 * So there is exactly one message for 401 here, it names neither field as the
 * wrong one, and the sign-up link under the form is unconditional — it reads the
 * same after a failed attempt as before one.
 *
 * # Registration discloses, on purpose, and only there
 *
 * `POST /auth/register` answers 409 for an address that is taken. That is the
 * API's deliberate trade (see `registerHandler`'s comment: the alternative needs
 * a mailer this service does not have, and a silent 201 leaves a user unable to
 * tell a typo from a taken address). This module relays it plainly, because
 * softening it would produce a worse outcome without recovering anything —
 * login, the endpoint an attacker actually enumerates against at scale, still
 * gives nothing away.
 */

/**
 * What went wrong, as a closed set the screens branch on.
 *
 * - `invalid_input` — 400. The API described which submitted value it rejected;
 *   safe to show verbatim, because it describes the request, not stored state.
 * - `invalid_credentials` — 401. One message, whatever the real reason.
 * - `no_organization` — 403 from the API. The account exists and the password
 *   was right, and it belongs to no organization. See {@link NO_ORGANIZATION}.
 * - `blocked` — 403 from *this* app's own CSRF guard. A different thing entirely
 *   from the one above, which is why it is a separate kind.
 * - `email_taken` — 409 on registration.
 * - `organization_exists` — 409 on the recovery call. Not a failure the user
 *   caused and not one they can act on except by signing in. See
 *   {@link ORGANIZATION_EXISTS}.
 * - `rate_limited` — 429, carrying `Retry-After`.
 * - `unconfirmed` — a registration that failed after the request was sent, where
 *   we cannot tell whether an account was created. See {@link UNCONFIRMED}.
 * - `unavailable` — anything else: the API is down, or answered something this
 *   app could not use.
 */
export type AuthFailureKind =
  | "invalid_input"
  | "invalid_credentials"
  | "no_organization"
  | "blocked"
  | "email_taken"
  | "organization_exists"
  | "rate_limited"
  | "unconfirmed"
  | "unavailable";

export type AuthFailure = {
  kind: AuthFailureKind;
  /** The sentence to show. Always present, never a stack trace. */
  message: string;
  /** Seconds to wait, present only for `rate_limited` and only when sent. */
  retryAfterSeconds?: number;
};

/**
 * Response header this app's own routes use to mark a same-origin refusal.
 *
 * Both a CSRF refusal and "this account has no organization" are 403, and they
 * need completely different screens, so the status alone is not enough to tell
 * them apart. The alternative — matching on the message text — would make the
 * copy load-bearing. `relayApiError` never sets this, so its presence means the
 * refusal came from this app rather than from the Go API.
 */
export const REFUSAL_HEADER = "x-collabboard-refusal";

/** The value {@link REFUSAL_HEADER} carries for a failed same-origin check. */
export const REFUSAL_CROSS_ORIGIN = "cross-origin";

/**
 * What a user sees when their account belongs to no organization.
 *
 * This is the visible face of issue #34. `Register` in `apps/api` commits the
 * user and its password in one transaction and the organization and membership
 * in a second, so a failure between the two leaves an account that can
 * authenticate and has nowhere to be. `Login` reports it as `ErrNoOrganization`
 * and the HTTP layer answers 403.
 *
 * Three things have to be true in this sentence, and two of them have not
 * changed: the credentials were right (so no "try again"), and signing up again
 * will not help (it would 409 on the address that already exists). That second
 * one is still the actual harm to avoid, so it is still said out loud.
 *
 * The third has changed, which is why this constant moved. It used to end in
 * "contact support", because the fix genuinely was not something the user could
 * perform — nothing created an organization for an existing account, and the
 * pre-tenant path has no delete, so an operator with a `psql` prompt was the
 * only exit. `POST /api/v1/organizations` (issue #34, ADR 0009) is that exit
 * now, and it needs no operator. Telling someone to open a support ticket for
 * something a button on the page in front of them does is worse than saying
 * nothing: it is a dead end that reads like a policy.
 *
 * The sentence names the affordance rather than describing a mechanism, and it
 * promises the sign-in that follows, because `WorkspaceRecovery` does perform it
 * — see that component for what happens when the follow-up login is the thing
 * that fails.
 */
export const NO_ORGANIZATION =
  "Your account exists, but it is not attached to a workspace, so there is nothing to sign in to. This happens when sign-up is interrupted part-way. Signing up again with this address will not fix it — create the missing workspace here instead, and we will sign you in as soon as it exists.";

/**
 * What a user sees when the recovery call answers 409.
 *
 * The account already has an organization, which means the situation resolved
 * itself between the 403 that offered the button and the click on it: another
 * tab did it, or two clicks raced. `Service.CreateFirstOrganization` scopes
 * itself to the zero-organization case and refuses anything else, and ADR 0009
 * is explicit that the guarantee is "an account that already has an organization
 * is refused" rather than "an account can never come to have two".
 *
 * So this is not an error to report, and the copy does not read like one. The
 * workspace the user was trying to create exists; the only thing left to do is
 * sign in, which the form they are already looking at will do.
 */
export const ORGANIZATION_EXISTS =
  "This account already has a workspace — it looks like one was just created, perhaps in another tab. Nothing further is needed: sign in below to open it.";

/**
 * What a user sees when registration failed in a way we cannot interpret.
 *
 * A 5xx from `POST /auth/register` covers three different states — the account
 * was never created, the account was created but its workspace was not (#34), or
 * the account was created and the answer was lost on the way back — and nothing
 * in the response distinguishes them. So the copy does not claim to know. It
 * gives the one instruction that is correct in all three: try signing in, and
 * do not sign up again, because two of the three states turn a retry into a 409.
 */
export const UNCONFIRMED =
  "Something went wrong and we could not confirm your account was created. Try signing in with the details you just entered. If that says your account has no workspace, contact support — do not sign up again, because the address may already be taken.";

const INVALID_CREDENTIALS = "Email or password is incorrect.";

const UNAVAILABLE =
  "We could not reach the server. Check your connection and try again in a moment.";

const BLOCKED =
  "That request did not look like it came from this site, so it was refused. Reload the page and try again.";

/** Parses the `Retry-After` seconds form. The API only ever sends that one. */
export function retryAfterSeconds(header: string | null): number | undefined {
  if (header === null) {
    return undefined;
  }

  const seconds = Number(header.trim());

  return Number.isFinite(seconds) && seconds >= 0 ? Math.ceil(seconds) : undefined;
}

/**
 * Renders a wait as something a person would say.
 *
 * Rounded up and made vague on purpose. A precise countdown in the message
 * would be a live region updating once a second, which a screen reader reads
 * aloud once a second.
 */
export function describeWait(seconds: number | undefined): string {
  if (seconds === undefined || seconds <= 0) {
    return "in a moment";
  }

  if (seconds < 60) {
    return `in about ${seconds} second${seconds === 1 ? "" : "s"}`;
  }

  const minutes = Math.ceil(seconds / 60);

  return `in about ${minutes} minute${minutes === 1 ? "" : "s"}`;
}

function rateLimited(retryAfter: string | null, subject: string): AuthFailure {
  const seconds = retryAfterSeconds(retryAfter);
  const failure: AuthFailure = {
    kind: "rate_limited",
    message: `Too many ${subject} attempts. Try again ${describeWait(seconds)}.`,
  };

  return seconds === undefined ? failure : { ...failure, retryAfterSeconds: seconds };
}

function blockedByOriginGuard(headers: Headers): boolean {
  return headers.get(REFUSAL_HEADER) === REFUSAL_CROSS_ORIGIN;
}

/**
 * Maps a failed `POST /api/auth/login`.
 *
 * `apiMessage` is the `error` string from the response body, or null when the
 * body was not this app's envelope. It is only ever shown for 400, where it
 * describes what was submitted; every other status gets copy from this module,
 * so a future change to a server-side message cannot quietly turn into a
 * different promise on the sign-in screen.
 */
export function describeLoginFailure(
  status: number,
  apiMessage: string | null,
  headers: Headers,
): AuthFailure {
  switch (status) {
    case 400:
      return {
        kind: "invalid_input",
        message: apiMessage ?? "Check the details you entered and try again.",
      };

    case 401:
      return { kind: "invalid_credentials", message: INVALID_CREDENTIALS };

    case 403:
      return blockedByOriginGuard(headers)
        ? { kind: "blocked", message: BLOCKED }
        : { kind: "no_organization", message: NO_ORGANIZATION };

    case 429:
      return rateLimited(headers.get("retry-after"), "sign-in");

    default:
      return { kind: "unavailable", message: UNAVAILABLE };
  }
}

/**
 * Maps a failed `POST /api/auth/first-organization`.
 *
 * # 403 cannot mean "no organization" here, and that is not a guess
 *
 * On login, a bare 403 is the API saying the account has no workspace. On this
 * route it is unreachable: `CreateFirstOrganization` can only return a rate-limit
 * error, `ErrInvalidCredentials`, `ErrAlreadyHasOrganization`, or something that
 * falls through to a 500 — `ErrNoOrganization` is the *precondition* of the call,
 * not one of its answers. So the only 403 that can arrive is this app's own CSRF
 * refusal, and an unmarked one is something we do not understand rather than a
 * diagnosis. That is why the two branches below are `blocked` and `unavailable`,
 * matching {@link describeRegistrationFailure} rather than
 * {@link describeLoginFailure}.
 *
 * # The 429 says "sign-in" on purpose
 *
 * This route is charged against the *login* budget — the same per-account and
 * per-address counters, under the same `auth:login:` key prefixes — and it is
 * charged before the credential is checked. A user who has just failed a few
 * sign-in attempts can therefore be refused on their very first click here,
 * having done nothing wrong at this button. Naming the wait after the sign-in
 * attempts that actually consumed the budget is the only version of that message
 * which explains itself; "too many workspace attempts" would be a lie about a
 * counter the user never touched.
 */
export function describeFirstOrganizationFailure(
  status: number,
  apiMessage: string | null,
  headers: Headers,
): AuthFailure {
  switch (status) {
    case 400:
      return {
        kind: "invalid_input",
        message: apiMessage ?? "Check the details you entered and try again.",
      };

    case 401:
      // Reachable only if the password was edited after the 403 that offered
      // this — the 403 itself is proof the credential verified a moment ago. One
      // message, same as login, for the same anti-enumeration reason.
      return { kind: "invalid_credentials", message: INVALID_CREDENTIALS };

    case 403:
      return blockedByOriginGuard(headers)
        ? { kind: "blocked", message: BLOCKED }
        : { kind: "unavailable", message: UNAVAILABLE };

    case 409:
      return { kind: "organization_exists", message: ORGANIZATION_EXISTS };

    case 429:
      return rateLimited(headers.get("retry-after"), "sign-in");

    default:
      return { kind: "unavailable", message: UNAVAILABLE };
  }
}

/** Maps a failed `POST /api/auth/register`. */
export function describeRegistrationFailure(
  status: number,
  apiMessage: string | null,
  headers: Headers,
): AuthFailure {
  switch (status) {
    case 400:
      return {
        kind: "invalid_input",
        message: apiMessage ?? "Check the details you entered and try again.",
      };

    case 403:
      return blockedByOriginGuard(headers)
        ? { kind: "blocked", message: BLOCKED }
        : { kind: "unavailable", message: UNAVAILABLE };

    case 409:
      return {
        kind: "email_taken",
        message: "An account already exists for that email address.",
      };

    case 429:
      return rateLimited(headers.get("retry-after"), "sign-up");

    default:
      // Everything at or above 500 — including the 502 `relayStatus` produces
      // for a request that never got an answer — lands here, because none of
      // them can tell us whether an account now exists.
      return status >= 500
        ? { kind: "unconfirmed", message: UNCONFIRMED }
        : { kind: "unavailable", message: UNAVAILABLE };
  }
}

/**
 * Reads the `{"error": "..."}` envelope, tolerating anything else.
 *
 * A body that is not JSON, or is JSON without a usable `error`, yields null and
 * the caller falls back to its own copy — so an HTML error page from a proxy
 * cannot become the sentence shown to a user. Same rule as
 * `lib/api/errors.ts`'s `parseErrorBody`, restated here because this module has
 * to stay importable from a Client Component.
 */
export async function readErrorMessage(response: Response): Promise<string | null> {
  let body: unknown;

  try {
    body = await response.json();
  } catch {
    return null;
  }

  if (typeof body !== "object" || body === null || Array.isArray(body)) {
    return null;
  }

  const message = (body as { error?: unknown }).error;

  return typeof message === "string" && message.trim() !== "" ? message.trim() : null;
}

/** The failure to show when `fetch` itself rejected — offline, usually. */
export function networkFailure(): AuthFailure {
  return { kind: "unavailable", message: UNAVAILABLE };
}
