/**
 * Where the auth screens live, and the one function that decides where a
 * visitor is sent afterwards.
 *
 * # Return-to is an open redirect waiting to happen
 *
 * "Send them back to the page they wanted" means taking a destination from a
 * query parameter — that is, from an attacker, since a query parameter is just
 * a link someone can send. `https://collabboard/login?next=https://evil/` on a
 * naive implementation is a phishing page with a real sign-in form in front of
 * it and the victim's own bank of trust behind it.
 *
 * {@link safeReturnPath} is therefore a whitelist of *shapes*, not a blacklist
 * of bad ones, and it is the only thing in this app that turns an untrusted
 * string into a navigation target. Everything it does not recognise becomes
 * {@link DEFAULT_SIGNED_IN_PATH}, which is a real page — a rejected value never
 * produces an error, just an ordinary landing.
 *
 * The four shapes it refuses and why:
 *
 * - anything not starting with `/` — `https://evil/`, `javascript:…`, `evil.com`
 * - `//evil.com` — a protocol-relative URL, which is absolute despite the
 *   leading slash, and the classic bypass of a naive `startsWith("/")` check
 * - `/\evil.com` — the same trick with a backslash, which several browsers and
 *   URL parsers normalise to `//`
 * - the auth screens themselves — not a security problem, but signing in and
 *   landing back on the sign-in form is a bug report
 */

/** Where a signed-in visitor lands when there is nowhere better to send them. */
export const DEFAULT_SIGNED_IN_PATH = "/app";

export const SIGN_IN_PATH = "/login";
export const SIGN_UP_PATH = "/register";

/** The query parameter carrying the page the visitor was trying to reach. */
export const RETURN_TO_PARAM = "next";

const AUTH_PATHS = [SIGN_IN_PATH, SIGN_UP_PATH];

/**
 * Turns an untrusted destination into one this app is willing to navigate to.
 *
 * Accepts a same-origin absolute path, with an optional query and fragment.
 * Returns {@link DEFAULT_SIGNED_IN_PATH} for everything else.
 */
export function safeReturnPath(raw: string | null | undefined): string {
  if (typeof raw !== "string" || raw === "") {
    return DEFAULT_SIGNED_IN_PATH;
  }

  // Anything at or below a space. A control character in a path is an encoding
  // bug or an attempt at header injection, and un-encoded whitespace is not a
  // path this app serves — refusing the whole range is one rule instead of two.
  if (/[\u0000-\u0020\u007f]/.test(raw)) {
    return DEFAULT_SIGNED_IN_PATH;
  }

  if (!raw.startsWith("/")) {
    return DEFAULT_SIGNED_IN_PATH;
  }

  // `//host` and `/\host` are absolute URLs wearing a leading slash.
  if (raw.startsWith("//") || raw.startsWith("/\\")) {
    return DEFAULT_SIGNED_IN_PATH;
  }

  const pathname = raw.split(/[?#]/, 1)[0];

  if (AUTH_PATHS.includes(pathname)) {
    return DEFAULT_SIGNED_IN_PATH;
  }

  return raw;
}

/**
 * The sign-in URL for a visitor who was trying to reach `returnTo`.
 *
 * The parameter is omitted when the destination is the default, so the common
 * case is a clean `/login` rather than `/login?next=%2Fapp`.
 */
export function signInHref(returnTo?: string | null): string {
  const target = safeReturnPath(returnTo);

  if (target === DEFAULT_SIGNED_IN_PATH) {
    return SIGN_IN_PATH;
  }

  return `${SIGN_IN_PATH}?${RETURN_TO_PARAM}=${encodeURIComponent(target)}`;
}

/** The sign-up URL, carrying the same destination through. */
export function signUpHref(returnTo?: string | null): string {
  const target = safeReturnPath(returnTo);

  if (target === DEFAULT_SIGNED_IN_PATH) {
    return SIGN_UP_PATH;
  }

  return `${SIGN_UP_PATH}?${RETURN_TO_PARAM}=${encodeURIComponent(target)}`;
}
