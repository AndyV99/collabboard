import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { API_URL_ENV, DEFAULT_API_BASE_URL } from "@/lib/api";
import { parseHealth, probeHealth } from "@/lib/health";

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
});

// Base URL resolution itself is covered in api.test.ts.

describe("parseHealth", () => {
  it("accepts the documented /healthz payload", () => {
    expect(
      parseHealth({
        status: "ok",
        components: { postgres: { status: "ok" }, redis: { status: "ok" } },
      }),
    ).toEqual({
      status: "ok",
      components: { postgres: { status: "ok" }, redis: { status: "ok" } },
    });
  });

  it("keeps per-component error strings from a degraded payload", () => {
    expect(
      parseHealth({
        status: "unavailable",
        components: { redis: { status: "unavailable", error: "boom" } },
      }),
    ).toEqual({
      status: "unavailable",
      components: { redis: { status: "unavailable", error: "boom" } },
    });
  });

  it.each([
    ["null", null],
    ["a string", "ok"],
    ["an array", []],
    ["a payload without components", { status: "ok" }],
    ["a payload with a non-string status", { status: 1, components: {} }],
    [
      "a component that is not an object",
      { status: "ok", components: { redis: "ok" } },
    ],
  ])("rejects %s", (_label, value) => {
    expect(parseHealth(value)).toBeNull();
  });
});

describe("probeHealth", () => {
  // Pin the base URL so the suite does not depend on the developer's shell.
  beforeEach(() => {
    vi.stubEnv(API_URL_ENV, undefined);
  });

  it("reports a healthy API", async () => {
    const fetchImpl = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          status: "ok",
          components: { postgres: { status: "ok" }, redis: { status: "ok" } },
        },
        200,
      ),
    );

    const probe = await probeHealth(fetchImpl);

    expect(fetchImpl).toHaveBeenCalledOnce();
    expect(fetchImpl.mock.calls[0][0]).toBe(
      `${DEFAULT_API_BASE_URL}/healthz`,
    );
    expect(probe).toMatchObject({ outcome: "reachable", httpStatus: 200 });
  });

  it("treats a 503 as a readable degraded response, not an error", async () => {
    const body = {
      status: "unavailable",
      components: {
        postgres: { status: "ok" },
        redis: { status: "unavailable", error: "connection refused" },
      },
    };
    const fetchImpl = vi.fn().mockResolvedValue(jsonResponse(body, 503));
    vi.spyOn(console, "warn").mockImplementation(() => {});

    const probe = await probeHealth(fetchImpl);

    expect(probe).toEqual({
      outcome: "reachable",
      url: `${DEFAULT_API_BASE_URL}/healthz`,
      httpStatus: 503,
      health: body,
    });
  });

  it("returns unreachable instead of throwing when the fetch fails", async () => {
    const fetchImpl = vi.fn().mockRejectedValue(new Error("fetch failed"));
    vi.spyOn(console, "error").mockImplementation(() => {});

    await expect(probeHealth(fetchImpl)).resolves.toEqual({
      outcome: "unreachable",
      url: `${DEFAULT_API_BASE_URL}/healthz`,
      error: "fetch failed",
    });
  });

  it("returns malformed when the body is not JSON", async () => {
    const fetchImpl = vi
      .fn()
      .mockResolvedValue(new Response("<html>nope</html>", { status: 200 }));
    vi.spyOn(console, "error").mockImplementation(() => {});

    const probe = await probeHealth(fetchImpl);

    expect(probe).toMatchObject({ outcome: "malformed", httpStatus: 200 });
  });

  it("returns malformed when the JSON is not the /healthz shape", async () => {
    const fetchImpl = vi
      .fn()
      .mockResolvedValue(jsonResponse({ message: "not found" }, 404));
    vi.spyOn(console, "error").mockImplementation(() => {});

    const probe = await probeHealth(fetchImpl);

    expect(probe).toMatchObject({ outcome: "malformed", httpStatus: 404 });
  });
});
