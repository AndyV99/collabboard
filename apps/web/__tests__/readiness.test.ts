import { afterEach, describe, expect, it, vi } from "vitest";

import { API_URL_ENV } from "@/lib/api";
import { SERVICE_NAME } from "@/lib/log";
import { evaluateReadiness } from "@/lib/readiness";

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

describe("evaluateReadiness", () => {
  it("is ready with a configured API URL", () => {
    const result = evaluateReadiness({
      [API_URL_ENV]: "https://api.staging.example.com",
    });

    expect(result.httpStatus).toBe(200);
    expect(result.detail).toBeNull();
    expect(result.body).toEqual({
      status: "ok",
      service: SERVICE_NAME,
      components: { api_url_config: { status: "ok", source: "env" } },
    });
  });

  // The most likely real misconfiguration: the task definition never set the
  // variable, so the container is quietly talking to its own localhost. Still
  // ready — nothing is broken — but `source` is how an operator spots it.
  it("is ready on the localhost default, and says the value came from the default", () => {
    const result = evaluateReadiness({});

    expect(result.httpStatus).toBe(200);
    expect(result.body.components.api_url_config).toEqual({
      status: "ok",
      source: "default",
    });
  });

  it("is not ready when the configured URL is malformed", () => {
    const result = evaluateReadiness({ [API_URL_ENV]: "${API_URL}" });

    expect(result.httpStatus).toBe(503);
    expect(result.body).toEqual({
      status: "unavailable",
      service: SERVICE_NAME,
      components: { api_url_config: { status: "unavailable" } },
    });
  });

  it("routes the offending value to the log detail, never the served body", () => {
    // #31 in one assertion: the operator gets the value, the anonymous caller
    // gets the verdict.
    const result = evaluateReadiness({
      [API_URL_ENV]: "http://internal-api.svc.cluster.local:8080?token=hunter2",
    });

    expect(result.detail?.env_var).toBe(API_URL_ENV);
    expect(result.detail?.error).toContain("internal-api.svc.cluster.local");
    expect(JSON.stringify(result.body)).not.toContain("internal-api");
    expect(JSON.stringify(result.body)).not.toContain("hunter2");
  });

  // The healthy payload is served to whoever can reach the route, so assert the
  // absence of topology positively rather than trusting the shape not to grow
  // a convenient `api_base_url` field later.
  it("never discloses the resolved base URL, even when healthy", () => {
    const result = evaluateReadiness({
      [API_URL_ENV]: "https://api.internal.example.com:8443/v1",
    });

    const serialised = JSON.stringify(result.body);

    expect(serialised).not.toContain("api.internal.example.com");
    expect(serialised).not.toContain("8443");
  });

  // Readiness is the web tier's own state. If it probed the API, an API outage
  // would deregister every web task and take the degraded page down with it.
  it("performs no network I/O", () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");

    evaluateReadiness({ [API_URL_ENV]: "https://api.example.com" });
    evaluateReadiness({ [API_URL_ENV]: "nonsense" });

    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("reads process.env when no environment is passed", () => {
    vi.stubEnv(API_URL_ENV, "https://api.from-process-env.example.com");

    expect(evaluateReadiness().body.components.api_url_config.source).toBe("env");
  });
});
