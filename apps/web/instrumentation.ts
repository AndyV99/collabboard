import { API_URL_ENV, apiBaseUrl, apiBaseUrlSource } from "@/lib/api";
import { logEvent } from "@/lib/log";

/**
 * Runs once per server instance, before the server accepts requests.
 *
 * Two jobs, both about the runtime configuration contract in `lib/api.ts`:
 *
 * 1. **Fail fast, and fail as a dead process.** `apiBaseUrl()` throws on a
 *    malformed `API_URL`. Next's own handling of a throwing instrumentation
 *    hook is to bind the port anyway and answer every request with a 500 —
 *    which reads exactly like the API being down, and can look alive to a
 *    health check that only tests the TCP connect. Exiting non-zero instead
 *    makes the container fail to start, which is what an ECS deployment
 *    circuit breaker watches for, and it is the same behaviour locally.
 * 2. **Say what it resolved.** One structured line naming the target makes
 *    "which API is this task actually talking to?" answerable from logs — the
 *    question that arrives the moment one image runs in two environments.
 */
export function register() {
  let url: string;

  try {
    url = apiBaseUrl();
  } catch (error) {
    logEvent("error", "invalid api base url configuration", {
      env_var: API_URL_ENV,
      error: error instanceof Error ? error.message : String(error),
    });

    // `instrumentation.ts` is also analysed for the Edge runtime, which has no
    // `process.exit`. Reaching it through `globalThis` keeps the reference out
    // of the Edge bundle's static analysis (a literal `process.exit(...)` here
    // makes `next build` emit an unsupported-API warning even though this app
    // has no Edge routes), and the runtime check below is the real guard.
    const nodeProcess = (globalThis as { process?: { exit?: (code: number) => never } })
      .process;

    if (typeof nodeProcess?.exit === "function") {
      nodeProcess.exit(1);
    }

    throw error;
  }

  logEvent("info", "resolved api base url", {
    api_base_url: url,
    source: apiBaseUrlSource(),
  });
}
