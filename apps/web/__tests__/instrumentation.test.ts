import { afterEach, describe, expect, it, vi } from "vitest";

import { register } from "@/instrumentation";
import { API_URL_ENV, DEFAULT_API_BASE_URL } from "@/lib/api";

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

/** Parses the single JSON line `logEvent` writes to the given console spy. */
function loggedLine(spy: { mock: { calls: unknown[][] } }): Record<string, unknown> {
  expect(spy.mock.calls).toHaveLength(1);

  return JSON.parse(String(spy.mock.calls[0][0])) as Record<string, unknown>;
}

describe("register", () => {
  it("logs the resolved target so a running task can be traced to its API", () => {
    vi.stubEnv(API_URL_ENV, "https://api.staging.example.com/");
    const info = vi.spyOn(console, "info").mockImplementation(() => {});

    register();

    expect(loggedLine(info)).toMatchObject({
      level: "info",
      message: "resolved api base url",
      api_base_url: "https://api.staging.example.com",
      source: "env",
    });
  });

  it("reports the localhost fallback as coming from the default, not the environment", () => {
    vi.stubEnv(API_URL_ENV, undefined);
    const info = vi.spyOn(console, "info").mockImplementation(() => {});

    register();

    expect(loggedLine(info)).toMatchObject({
      api_base_url: DEFAULT_API_BASE_URL,
      source: "default",
    });
  });

  it("logs and exits non-zero on a malformed value rather than starting", () => {
    // The failure that matters: Next's default handling of a throwing
    // instrumentation hook is to bind the port and serve 500s, which looks
    // like an API outage. This asserts the process dies instead.
    vi.stubEnv(API_URL_ENV, "not-a-url");
    const error = vi.spyOn(console, "error").mockImplementation(() => {});
    // The real `process.exit` never returns, so the stub must not either —
    // otherwise the test exercises a control flow that cannot happen.
    const exit = vi.spyOn(process, "exit").mockImplementation((() => {
      throw new Error("process.exit");
    }) as never);

    expect(() => register()).toThrow("process.exit");
    expect(exit).toHaveBeenCalledExactlyOnceWith(1);

    const line = loggedLine(error);

    expect(line).toMatchObject({
      level: "error",
      message: "invalid api base url configuration",
      env_var: API_URL_ENV,
    });
    expect(String(line.error)).toContain("not-a-url");
  });

  it("rethrows if the runtime has no process.exit to call", () => {
    vi.stubEnv(API_URL_ENV, "not-a-url");
    vi.spyOn(console, "error").mockImplementation(() => {});

    // Stands in for the Edge runtime, where `process.exit` does not exist.
    const exit = process.exit;
    Reflect.deleteProperty(process, "exit");

    try {
      expect(() => register()).toThrow(/not-a-url/);
    } finally {
      process.exit = exit;
    }
  });
});
