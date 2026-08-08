import { apiBaseUrl } from "@/lib/api";
import { logEvent } from "@/lib/log";

/** Path of the API's health endpoint (see `apps/api/internal/api/health.go`). */
export const HEALTH_PATH = "/healthz";

/**
 * Upper bound on the health request. The API bounds each dependency probe at
 * 2s, so anything past this is the API itself being unresponsive — and a page
 * render must not hang on it.
 */
export const HEALTH_TIMEOUT_MS = 5_000;

/** Status value the API reports when everything it depends on is reachable. */
export const STATUS_OK = "ok";

/** Per-dependency block of the API's `/healthz` payload. */
export type ComponentHealth = {
  status: string;
  error?: string;
};

/** The API's `/healthz` payload. */
export type ApiHealth = {
  status: string;
  components: Record<string, ComponentHealth>;
};

/**
 * Outcome of a health probe. Split three ways because the failure modes need
 * different words in the UI: the API answering "unavailable" is a working
 * web-to-API path reporting a real dependency outage, whereas an unreachable or
 * unrecognisable API means we learned nothing about the dependencies at all.
 */
export type HealthProbe =
  | { outcome: "reachable"; url: string; httpStatus: number; health: ApiHealth }
  | { outcome: "malformed"; url: string; httpStatus: number; error: string }
  | { outcome: "unreachable"; url: string; error: string };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseComponent(value: unknown): ComponentHealth | null {
  if (!isRecord(value) || typeof value.status !== "string") {
    return null;
  }

  const error = value.error;

  if (error !== undefined && typeof error !== "string") {
    return null;
  }

  return error === undefined
    ? { status: value.status }
    : { status: value.status, error };
}

/**
 * Validates an arbitrary JSON payload against the shape `/healthz` promises.
 * Returns null rather than throwing, so a wrong `NEXT_PUBLIC_API_URL` pointing
 * at some other service degrades into a rendered message instead of a crash.
 */
export function parseHealth(value: unknown): ApiHealth | null {
  if (!isRecord(value) || typeof value.status !== "string") {
    return null;
  }

  if (!isRecord(value.components)) {
    return null;
  }

  const components: Record<string, ComponentHealth> = {};

  for (const [name, raw] of Object.entries(value.components)) {
    const component = parseComponent(raw);

    if (component === null) {
      return null;
    }

    components[name] = component;
  }

  return { status: value.status, components };
}

/**
 * Fetches `/healthz` from the API. Never throws: every failure mode is folded
 * into a `HealthProbe` the page can render.
 *
 * `fetchImpl` is injectable so the behaviour can be unit tested without
 * stubbing globals or standing up a server.
 */
export async function probeHealth(
  fetchImpl: typeof fetch = fetch,
): Promise<HealthProbe> {
  const url = `${apiBaseUrl()}${HEALTH_PATH}`;

  let response: Response;

  try {
    response = await fetchImpl(url, {
      cache: "no-store",
      headers: { accept: "application/json" },
      signal: AbortSignal.timeout(HEALTH_TIMEOUT_MS),
    });
  } catch (cause) {
    const error = cause instanceof Error ? cause.message : String(cause);

    logEvent("error", "api health probe failed", { url, error });

    return { outcome: "unreachable", url, error };
  }

  let payload: unknown;

  try {
    payload = await response.json();
  } catch {
    logEvent("error", "api health response was not json", {
      url,
      http_status: response.status,
    });

    return {
      outcome: "malformed",
      url,
      httpStatus: response.status,
      error: "response body was not JSON",
    };
  }

  const health = parseHealth(payload);

  if (health === null) {
    logEvent("error", "api health response had an unexpected shape", {
      url,
      http_status: response.status,
    });

    return {
      outcome: "malformed",
      url,
      httpStatus: response.status,
      error: "response JSON did not match the /healthz schema",
    };
  }

  if (health.status !== STATUS_OK) {
    logEvent("warn", "api reported degraded health", {
      url,
      http_status: response.status,
      api_status: health.status,
    });
  }

  return { outcome: "reachable", url, httpStatus: response.status, health };
}
