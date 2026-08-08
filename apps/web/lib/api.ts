/**
 * Name of the environment variable carrying the API base URL.
 *
 * Deliberately NOT prefixed `NEXT_PUBLIC_`. Next replaces every literal
 * `process.env.NEXT_PUBLIC_*` reference with its value during `next build`, so
 * a public variable is frozen into the artifact and the same image cannot be
 * promoted dev → staging → prod with different targets. A server-only name is
 * read from the live process environment on every request instead, which is
 * what the ECS promotion model in the vault's infrastructure section needs.
 *
 * Read through a computed lookup (`env[API_URL_ENV]`) rather than a literal
 * member expression, so no bundler can constant-fold it even if the variable is
 * ever renamed into the public namespace by mistake.
 */
export const API_URL_ENV = "API_URL";

/**
 * Base URL used when `API_URL` is not set at all. Matches the compose stack in
 * the repo-root `CLAUDE.md`, where the web app talks to the API on
 * localhost:8080 in dev — so `npm run dev` needs no `.env.local`.
 */
export const DEFAULT_API_BASE_URL = "http://localhost:8080";

/**
 * Thrown when `API_URL` is present but unusable.
 *
 * A distinct type rather than a bare `Error` so a caller (today: the
 * `instrumentation.ts` boot check) can tell "this deployment is misconfigured"
 * apart from "the API happens to be down", which are different pages to wake
 * someone for.
 */
export class ApiUrlConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ApiUrlConfigError";
  }
}

function invalid(value: string, reason: string): ApiUrlConfigError {
  return new ApiUrlConfigError(
    `${API_URL_ENV} is set to ${JSON.stringify(value)}, which ${reason}. ` +
      `Expected an absolute http(s) URL such as "https://api.example.com" ` +
      `(a path prefix is allowed; a query string or fragment is not). ` +
      `Unset ${API_URL_ENV} to fall back to ${DEFAULT_API_BASE_URL} for local development.`,
  );
}

/**
 * Resolves the API base URL from the environment at call time.
 *
 * Unset means local development, so it falls back to the localhost default.
 * Set-but-unusable is a deployment mistake — an unsubstituted task-definition
 * placeholder, a copy-paste with a stray space — and throws rather than
 * quietly falling back, because a production container silently pointing at
 * `localhost:8080` fails much later and much less legibly than a container that
 * refuses to start.
 *
 * The return value is normalised (canonical origin, no trailing slash) so
 * callers can concatenate a leading-slash path without a double slash.
 *
 * `env` is injectable so the resolution rules are unit testable without
 * mutating the real process environment.
 */
export function apiBaseUrl(
  env: Readonly<Record<string, string | undefined>> = process.env,
): string {
  const raw = env[API_URL_ENV];

  if (raw === undefined) {
    return DEFAULT_API_BASE_URL;
  }

  const trimmed = raw.trim();

  if (trimmed === "") {
    throw invalid(raw, "is empty");
  }

  let parsed: URL;

  try {
    parsed = new URL(trimmed);
  } catch {
    throw invalid(raw, "is not an absolute URL");
  }

  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw invalid(raw, `uses the "${parsed.protocol}" scheme`);
  }

  if (parsed.search !== "" || parsed.hash !== "") {
    throw invalid(raw, "carries a query string or fragment");
  }

  return parsed.href.replace(/\/+$/, "");
}

/**
 * Whether the resolved base URL came from the environment or the built-in
 * default. Only used for the startup log line — a deployment that logs
 * `"source": "default"` is one where the environment variable never arrived.
 */
export function apiBaseUrlSource(
  env: Readonly<Record<string, string | undefined>> = process.env,
): "env" | "default" {
  return env[API_URL_ENV] === undefined ? "default" : "env";
}
