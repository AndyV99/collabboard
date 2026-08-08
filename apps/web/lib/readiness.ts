import { API_URL_ENV, apiBaseUrl, apiBaseUrlSource } from "@/lib/api";
import { SERVICE_NAME } from "@/lib/log";

/**
 * Readiness of the web tier — what a load balancer target-group health check
 * asks, and what `HEALTHCHECK` in the Dockerfile asks.
 *
 * ## What "ready" means here, and what it deliberately does not
 *
 * Ready means: **this process can serve a request, and its own configuration
 * resolved.** That is two facts, and both are the web tier's own:
 *
 * 1. Next booted far enough to route and render — answering at all proves
 *    this, which is why the check is an HTTP route and not a TCP probe. A
 *    bound port proves only that the kernel accepted a connection; a process
 *    wedged after `listen()` still passes that.
 * 2. `API_URL` resolves under the rules in `lib/api.ts`. This is the half `/`
 *    cannot report. `/` returns 200 with a degraded panel whether the config
 *    is sound or not, so it cannot distinguish "up" from "up and configured" —
 *    exactly the gap that makes it a poor readiness signal.
 *
 * Not ready does **not** mean the API is down. Reaching the Go API is
 * deliberately excluded:
 *
 * - The web app renders a degraded panel when the API is unreachable. That is
 *   designed behaviour, not failure — a page that says "the API is down" is
 *   the correct response to the API being down, and it can only be served by
 *   a task the load balancer still routes to.
 * - Otherwise an API outage deregisters every web task too, turning one tier's
 *   failure into both tiers' failure and taking the status page down with it.
 * - It would also make health checks a traffic source against the API: every
 *   task, every few seconds, from every environment.
 *
 * The API's own health is the API's target group's business. This route
 * therefore never calls it — see `lib/health.ts`, which is the page's probe and
 * is a different question with a different consumer.
 *
 * ## Disclosure (#31)
 *
 * This route is unauthenticated and, until the ALB restricts it, potentially
 * public. So the body names *what* is wrong, never *what the value was*:
 *
 * - The resolved base URL is not in the payload. It is internal topology — the
 *   API's hostname and port — and an anonymous caller has no use for it. The
 *   resolved URL is already emitted once per boot as a structured log line by
 *   `instrumentation.ts`, which is where an operator should read it from.
 * - `source` ("env" vs "default") *is* in the payload. It answers the single
 *   most likely misconfiguration — the task definition never set `API_URL`, so
 *   the task is quietly pointed at `localhost:8080` — and it discloses nothing:
 *   the variable's name and its default are both in this repo's README.
 * - When resolution fails, the thrown message (which quotes the bad value) is
 *   logged, not served. Same split the API is asked to make in #31: operator
 *   gets the detail, the internet gets the verdict.
 */

/** Value of `status`, on the payload as a whole and per component. */
export type ReadinessStatus = "ok" | "unavailable";

/**
 * Payload served by `GET /healthz`.
 *
 * Shaped like the Go API's `/healthz` (`status` + a `components` map) so the
 * two services read the same way in a dashboard, with `service` added because
 * a health payload that does not say who answered is ambiguous the moment two
 * of them are behind one hostname.
 */
export type ReadinessBody = {
  status: ReadinessStatus;
  service: string;
  components: {
    api_url_config: {
      status: ReadinessStatus;
      /** Only present when resolution succeeded — there is no source otherwise. */
      source?: "env" | "default";
    };
  };
};

/**
 * A readiness verdict, split into what is served and what is only logged.
 *
 * `detail` is the suppressed half: non-null exactly when something failed, and
 * never reachable from `body`. Keeping the split in the return type rather than
 * inside a logging call makes it a thing a test can assert on — that the served
 * body does not contain the text that went to the logs.
 */
export type ReadinessResult = {
  httpStatus: 200 | 503;
  body: ReadinessBody;
  detail: { env_var: string; error: string } | null;
};

/**
 * Evaluates readiness against an environment. Pure and total: it never throws
 * and never performs I/O, so the route handler cannot fail in a way that turns
 * a health check into a 500 — which reads as "app broken" rather than
 * "misconfigured", and is a different page for a different person.
 *
 * `env` is injectable for the same reason it is in `lib/api.ts`: the rules are
 * unit testable without mutating the real process environment.
 */
export function evaluateReadiness(
  env: Readonly<Record<string, string | undefined>> = process.env,
): ReadinessResult {
  try {
    apiBaseUrl(env);
  } catch (error) {
    return {
      httpStatus: 503,
      body: {
        status: "unavailable",
        service: SERVICE_NAME,
        components: { api_url_config: { status: "unavailable" } },
      },
      detail: {
        env_var: API_URL_ENV,
        error: error instanceof Error ? error.message : String(error),
      },
    };
  }

  return {
    httpStatus: 200,
    body: {
      status: "ok",
      service: SERVICE_NAME,
      components: {
        api_url_config: { status: "ok", source: apiBaseUrlSource(env) },
      },
    },
    detail: null,
  };
}
