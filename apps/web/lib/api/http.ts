/**
 * The one place a request is actually sent.
 *
 * Everything above this file describes *what* to ask for; this file turns a
 * description into a `fetch` and a response into an {@link ApiResult}. It knows
 * nothing about sessions, cookies or refresh — that lives in `lib/session` and
 * in the two transports (`lib/api/server.ts`, `lib/api/browser.ts`), so this
 * function stays testable without any of it.
 */

import {
  type ApiResult,
  err,
  errorFromStatus,
  malformedError,
  networkError,
  ok,
} from "./errors";

/**
 * Path prefix every resource route sits behind on the Go API (`router.go`:
 * `v1 := router.Group("/api/v1")`).
 *
 * Endpoint paths in this package are written *without* it — `/projects`, not
 * `/api/v1/projects` — so the same path can be sent to the Go API by the server
 * transport and to this app's own `/api/proxy` by the browser transport. One
 * definition of the route, two bases.
 */
export const API_V1_PREFIX = "/api/v1";

/**
 * Upper bound on a single API request.
 *
 * Matches the reasoning in `lib/health.ts`: a request that has not answered by
 * now is the API being unresponsive, and a Server Component render must not
 * hang on it. It is deliberately longer than the health probe's 5s because
 * these calls do real work.
 */
export const REQUEST_TIMEOUT_MS = 10_000;

/** HTTP methods this client uses. */
export type HttpMethod = "GET" | "POST" | "PATCH" | "DELETE";

/**
 * A described request, independent of where it will be sent.
 *
 * `parse` is what makes the result typed: it validates the decoded body and
 * returns null when it is not the promised shape, which becomes a `malformed`
 * error rather than a value the UI would go on to render as `undefined`.
 *
 * `expectNoContent` marks the endpoints that answer 204 (`DELETE /cards/:id`,
 * `POST /auth/logout`). Their `parse` is never called.
 */
export type Endpoint<T> = {
  method: HttpMethod;
  path: string;
  body?: unknown;
  parse: (value: unknown) => T | null;
  expectNoContent?: boolean;
};

/** How a transport is asked to run an {@link Endpoint}. */
export type ApiCall = <T>(endpoint: Endpoint<T>) => Promise<ApiResult<T>>;

/** Options for {@link sendRequest}. */
export type SendOptions = {
  /** Absolute base to prefix `endpoint.path` with. No trailing slash. */
  baseUrl: string;
  /** Bearer token, when the endpoint is authenticated. */
  accessToken?: string;
  /** Extra headers. Used by the browser transport for CSRF-adjacent hints. */
  headers?: Record<string, string>;
  /** Injectable for tests; defaults to the global `fetch`. */
  fetchImpl?: typeof fetch;
  /** Overrides {@link REQUEST_TIMEOUT_MS}. */
  timeoutMs?: number;
  /** Caller-supplied cancellation, combined with the timeout. */
  signal?: AbortSignal;
  /**
   * Cookie policy. Only the browser transport sets it, to `same-origin`, which
   * is already the default — stated explicitly because it is load-bearing: the
   * session cookies are what make `/api/proxy` authenticated.
   */
  credentials?: RequestCredentials;
};

/**
 * Sends one request and folds every outcome into an {@link ApiResult}.
 *
 * Never throws for a network failure, a non-2xx status, or an unparseable body.
 * It will throw if `parse` throws, which would be a bug in this package.
 */
export async function sendRequest<T>(
  endpoint: Endpoint<T>,
  options: SendOptions,
): Promise<ApiResult<T>> {
  const fetchImpl = options.fetchImpl ?? fetch;
  const url = `${options.baseUrl}${endpoint.path}`;

  const headers: Record<string, string> = {
    accept: "application/json",
    ...options.headers,
  };

  if (options.accessToken !== undefined && options.accessToken !== "") {
    headers.authorization = `Bearer ${options.accessToken}`;
  }

  const init: RequestInit = {
    method: endpoint.method,
    headers,
    // Health, boards and cards are all live facts. Next caches `fetch` in
    // Server Components unless told otherwise, and a cached authenticated
    // response is a cross-user data leak, not just a stale board.
    cache: "no-store",
    signal: timeoutSignal(options),
  };

  if (options.credentials !== undefined) {
    init.credentials = options.credentials;
  }

  if (endpoint.body !== undefined) {
    headers["content-type"] = "application/json";
    init.body = JSON.stringify(endpoint.body);
  }

  let response: Response;

  try {
    response = await fetchImpl(url, init);
  } catch {
    return err(networkError());
  }

  return readResponse(endpoint, response);
}

/**
 * Combines the caller's signal with the timeout.
 *
 * `AbortSignal.any` is used rather than a manual listener so the timer is
 * released when the caller aborts first.
 */
function timeoutSignal(options: SendOptions): AbortSignal {
  const timeout = AbortSignal.timeout(options.timeoutMs ?? REQUEST_TIMEOUT_MS);

  return options.signal === undefined
    ? timeout
    : AbortSignal.any([timeout, options.signal]);
}

async function readResponse<T>(
  endpoint: Endpoint<T>,
  response: Response,
): Promise<ApiResult<T>> {
  if (response.status === 204 || response.status === 205) {
    // A 204 for an endpoint that promised a body is a contract violation, and
    // handing `parse` an `undefined` would silently become `malformed` with a
    // less useful reason. Both go through the same door on purpose.
    return endpoint.expectNoContent === true
      ? ok(endpoint.parse(undefined) as T)
      : err(malformedError(response.status));
  }

  let payload: unknown;

  try {
    payload = await response.json();
  } catch {
    // A body-less error response is normal for a proxy or a gateway. The status
    // is still the truth, so an unreadable body on a failure keeps the status's
    // meaning; on a success it is malformed.
    return response.ok
      ? err(malformedError(response.status))
      : err(errorFromStatus(response.status, undefined, response.headers.get("retry-after")));
  }

  if (!response.ok) {
    return err(
      errorFromStatus(response.status, payload, response.headers.get("retry-after")),
    );
  }

  const parsed = endpoint.parse(payload);

  return parsed === null ? err(malformedError(response.status)) : ok(parsed);
}
