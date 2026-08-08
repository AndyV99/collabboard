/**
 * Base URL of the Go API (`apps/api`). Overridden per environment via
 * `NEXT_PUBLIC_API_URL`; the default matches the compose stack in `CLAUDE.md`,
 * where the web app talks to the API on localhost:8080 in dev.
 */
export const DEFAULT_API_BASE_URL = "http://localhost:8080";

/**
 * Resolves the API base URL, normalising away a trailing slash so callers can
 * concatenate a leading-slash path without producing a double slash.
 */
export function apiBaseUrl(
  env: Readonly<Record<string, string | undefined>> = process.env,
): string {
  const configured = env.NEXT_PUBLIC_API_URL?.trim();

  return (configured || DEFAULT_API_BASE_URL).replace(/\/+$/, "");
}
