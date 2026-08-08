import { afterEach, describe, expect, it, vi } from "vitest";

import { API_URL_ENV } from "@/lib/api";

// `connection()` needs Next's request store, which only exists inside a real
// request. Stubbing it keeps the handler itself — status code, headers, and the
// body/log split — testable without standing up a server. The container
// evidence in the README covers the wiring this cannot.
const connection = vi.hoisted(() => vi.fn(async () => {}));

vi.mock("next/server", () => ({ connection }));

const { GET } = await import("@/app/healthz/route");

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
  connection.mockClear();
});

describe("GET /healthz", () => {
  it("returns 200 and an uncacheable body when configuration resolves", async () => {
    vi.stubEnv(API_URL_ENV, "https://api.example.com");

    const response = await GET();

    expect(response.status).toBe(200);
    expect(response.headers.get("cache-control")).toBe("no-store");
    await expect(response.json()).resolves.toMatchObject({
      status: "ok",
      service: "collabboard-web",
    });
  });

  // Opting out of prerendering is the whole reason the route reflects the
  // running container's environment rather than the build machine's. A silent
  // removal of the call would be caught here and nowhere else in the suite.
  it("opts out of prerendering on every request", async () => {
    vi.stubEnv(API_URL_ENV, "https://api.example.com");

    await GET();

    expect(connection).toHaveBeenCalledOnce();
  });

  it("returns 503 and logs the suppressed detail when configuration is broken", async () => {
    vi.stubEnv(API_URL_ENV, "http://internal-api:8080?token=hunter2");
    const error = vi.spyOn(console, "error").mockImplementation(() => {});

    const response = await GET();

    expect(response.status).toBe(503);
    await expect(response.text()).resolves.not.toContain("hunter2");

    expect(error).toHaveBeenCalledOnce();
    const line = JSON.parse(String(error.mock.calls[0][0])) as Record<
      string,
      unknown
    >;
    expect(line).toMatchObject({
      level: "error",
      message: "readiness check failed",
      env_var: API_URL_ENV,
    });
    expect(String(line.error)).toContain("internal-api");
  });

  it("logs nothing when ready, so the check is not a log firehose", async () => {
    // A health check fires every few seconds per task. One line per probe would
    // bury everything else and cost real money in CloudWatch ingest.
    vi.stubEnv(API_URL_ENV, "https://api.example.com");
    const info = vi.spyOn(console, "info").mockImplementation(() => {});
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    const error = vi.spyOn(console, "error").mockImplementation(() => {});

    await GET();

    expect(info).not.toHaveBeenCalled();
    expect(warn).not.toHaveBeenCalled();
    expect(error).not.toHaveBeenCalled();
  });
});
