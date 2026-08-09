/**
 * The error half of the API client's result type.
 *
 * Nothing in `lib/api` throws for a failure the server described. A 404 is not
 * exceptional — it is one of the answers the endpoint has — and turning it into
 * a thrown value means every call site either wraps in try/catch or forgets to.
 * The client returns {@link ApiResult} instead, so a caller that ignores the
 * failure case does not compile.
 *
 * Thrown errors are still possible for programmer mistakes (a bad argument, a
 * bug in a parser). Those are bugs, not outcomes, and they should crash.
 */

/**
 * The closed set of failures a caller can branch on.
 *
 * One kind per response the UI has to treat differently, which is why 400 and
 * 422 collapse into `bad_request` but 401 and 403 do not: "sign in again" and
 * "you are signed in and still may not" are different screens.
 *
 * - `bad_request` — 400. The request was malformed or a field was rejected.
 * - `unauthorized` — 401. No usable session. After the client's one refresh
 *   attempt, this means the session is genuinely gone.
 * - `forbidden` — 403. Authenticated, not permitted. Never retried.
 * - `not_found` — 404. Includes another tenant's object: the API answers 404
 *   rather than 403 there on purpose, so this kind must not be read as "exists
 *   but is not yours".
 * - `conflict` — 409. The request disagrees with current state — a stale
 *   drag-and-drop, a duplicate registration.
 * - `rate_limited` — 429. Carries `retryAfterSeconds` when the API sent one.
 * - `server_error` — 5xx. The API failed; the detail is in its logs, not here.
 * - `unexpected_status` — a status this client has no mapping for.
 * - `network` — the request never produced a response: DNS, connection refused,
 *   TLS, or the client-side timeout.
 * - `malformed` — a response arrived but its body was not the shape the
 *   endpoint promises. Usually `API_URL` pointing at something that is not this
 *   API.
 */
export type ApiErrorKind =
  | "bad_request"
  | "unauthorized"
  | "forbidden"
  | "not_found"
  | "conflict"
  | "rate_limited"
  | "server_error"
  | "unexpected_status"
  | "network"
  | "malformed";

/**
 * A failure, as a value.
 *
 * `message` is safe to show a user: it is either the API's own `{"error": ...}`
 * string — which that service deliberately keeps free of stored state — or one
 * of the defaults below. It is never a stack trace and never the response body
 * of a 5xx, which this API defines as the constant "internal server error".
 *
 * `status` is null exactly when `kind` is `network`, because there was no
 * response to take a status from.
 */
export type ApiError = {
  kind: ApiErrorKind;
  message: string;
  status: number | null;
  /** Present only for `rate_limited`, and only when the API sent Retry-After. */
  retryAfterSeconds?: number;
};

/**
 * The result of every call in this package.
 *
 * A discriminated union rather than `T | null` so a caller cannot lose the
 * reason, and rather than a thrown error so the reason is in the type.
 */
export type ApiResult<T> =
  | { ok: true; data: T }
  | { ok: false; error: ApiError };

/** Wraps a value as a success. */
export function ok<T>(data: T): ApiResult<T> {
  return { ok: true, data };
}

/** Wraps an {@link ApiError} as a failure. */
export function err<T>(error: ApiError): ApiResult<T> {
  return { ok: false, error };
}

/**
 * Default message per kind, used when the API sent no usable `error` field.
 *
 * Written as user-facing sentences because that is where they end up. A caller
 * that wants its own wording branches on `kind`, which is the point of having
 * one.
 */
const DEFAULT_MESSAGES: Record<ApiErrorKind, string> = {
  bad_request: "The request was rejected.",
  unauthorized: "Your session has expired. Sign in again.",
  forbidden: "You do not have access to this.",
  not_found: "Not found.",
  conflict: "This changed while you were working on it. Reload and try again.",
  rate_limited: "Too many attempts. Try again shortly.",
  server_error: "Something went wrong on our side.",
  unexpected_status: "The server returned an unexpected response.",
  network: "Could not reach the server.",
  malformed: "The server returned an unexpected response.",
};

/**
 * Maps an HTTP status onto an {@link ApiErrorKind}.
 *
 * Exhaustive over the statuses `apps/api` actually produces — see
 * `internal/api/auth.go`'s `writeAuthError` and `internal/api/crud.go`'s
 * `writeStoreError`, which between them emit 400, 401, 403, 404, 409, 429 and
 * 500. Anything else lands on `unexpected_status` rather than being guessed at.
 */
export function kindForStatus(status: number): ApiErrorKind {
  switch (status) {
    case 400:
    case 422:
      return "bad_request";
    case 401:
      return "unauthorized";
    case 403:
      return "forbidden";
    case 404:
      return "not_found";
    case 409:
      return "conflict";
    case 429:
      return "rate_limited";
    default:
      return status >= 500 && status <= 599 ? "server_error" : "unexpected_status";
  }
}

/**
 * Reads the API's error envelope.
 *
 * `apps/api` returns exactly one error shape — `{"error": "..."}` — and this is
 * the only place that knows it. A body that is not that shape (an HTML error
 * page from a proxy, say) yields null and the caller falls back to a default,
 * so a load balancer's 502 page cannot become the message shown to a user.
 */
export function parseErrorBody(value: unknown): string | null {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    return null;
  }

  const message = (value as { error?: unknown }).error;

  return typeof message === "string" && message.trim() !== "" ? message : null;
}

/**
 * Parses a `Retry-After` header expressed in seconds.
 *
 * The API always sends the delta-seconds form (`strconv.Itoa` in
 * `writeAuthError`). The HTTP-date form is legal and not produced here, so it is
 * treated as absent rather than half-parsed into a wrong number.
 */
export function parseRetryAfter(header: string | null): number | undefined {
  if (header === null) {
    return undefined;
  }

  const seconds = Number(header.trim());

  return Number.isFinite(seconds) && seconds >= 0 ? seconds : undefined;
}

/** Builds an {@link ApiError} for a response the API described. */
export function errorFromStatus(
  status: number,
  body: unknown,
  retryAfter?: string | null,
): ApiError {
  const kind = kindForStatus(status);
  const message = parseErrorBody(body) ?? DEFAULT_MESSAGES[kind];

  if (kind !== "rate_limited") {
    return { kind, message, status };
  }

  const retryAfterSeconds = parseRetryAfter(retryAfter ?? null);

  return retryAfterSeconds === undefined
    ? { kind, message, status }
    : { kind, message, status, retryAfterSeconds };
}

/**
 * Builds an {@link ApiError} for a request that never got a response.
 *
 * `cause` is folded into a generic message rather than surfaced: a fetch
 * rejection can name the internal hostname and port it failed to reach, and
 * these messages are rendered to users. The detail belongs in the server log,
 * which is where the callers of this put it.
 */
export function networkError(): ApiError {
  return { kind: "network", message: DEFAULT_MESSAGES.network, status: null };
}

/** Builds an {@link ApiError} for a response whose body was the wrong shape. */
export function malformedError(status: number): ApiError {
  return { kind: "malformed", message: DEFAULT_MESSAGES.malformed, status };
}
