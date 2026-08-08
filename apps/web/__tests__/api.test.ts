import { afterEach, describe, expect, it, vi } from "vitest";

import {
  API_URL_ENV,
  ApiUrlConfigError,
  DEFAULT_API_BASE_URL,
  apiBaseUrl,
  apiBaseUrlSource,
} from "@/lib/api";

afterEach(() => {
  vi.unstubAllEnvs();
});

describe("apiBaseUrl", () => {
  it("falls back to the local API when the variable is unset", () => {
    expect(apiBaseUrl({})).toBe(DEFAULT_API_BASE_URL);
    expect(apiBaseUrlSource({})).toBe("default");
  });

  it("uses the configured URL", () => {
    expect(apiBaseUrl({ [API_URL_ENV]: "https://api.example.com" })).toBe(
      "https://api.example.com",
    );
    expect(apiBaseUrlSource({ [API_URL_ENV]: "https://api.example.com" })).toBe(
      "env",
    );
  });

  it("resolves a different value from the same code, which is the point", () => {
    // The runtime-config guarantee stated as a test: one module, two
    // environments, two answers. Nothing about the value is captured at import
    // time, so a built artifact behaves differently per deployment.
    expect(apiBaseUrl({ [API_URL_ENV]: "https://api.staging.example.com" })).toBe(
      "https://api.staging.example.com",
    );
    expect(apiBaseUrl({ [API_URL_ENV]: "https://api.prod.example.com" })).toBe(
      "https://api.prod.example.com",
    );
  });

  it.each([
    ["a trailing slash", "https://api.example.com/", "https://api.example.com"],
    ["several trailing slashes", "https://api.example.com///", "https://api.example.com"],
    ["surrounding whitespace", "  https://api.example.com  ", "https://api.example.com"],
    ["a path prefix behind a shared host", "https://example.com/api/", "https://example.com/api"],
    ["an explicit port", "http://api:8080", "http://api:8080"],
  ])("normalises %s", (_label, value, expected) => {
    expect(apiBaseUrl({ [API_URL_ENV]: value })).toBe(expected);
  });

  it("concatenates with a leading-slash path without doubling the slash", () => {
    expect(`${apiBaseUrl({ [API_URL_ENV]: "https://api.example.com/" })}/healthz`).toBe(
      "https://api.example.com/healthz",
    );
  });

  // Set-but-unusable is a deployment mistake, not a request to use the default:
  // silently falling back would point a production container at localhost.
  it.each([
    ["an empty value", ""],
    ["a whitespace-only value", "   "],
    ["a host with no scheme", "api.example.com"],
    ["a bare path", "/api"],
    ["an unsupported scheme", "ftp://api.example.com"],
    ["a websocket scheme", "wss://api.example.com"],
    ["a query string", "https://api.example.com?token=abc"],
    ["a fragment", "https://api.example.com#frag"],
    ["an unsubstituted placeholder", "${API_URL}"],
  ])("rejects %s", (_label, value) => {
    expect(() => apiBaseUrl({ [API_URL_ENV]: value })).toThrow(
      ApiUrlConfigError,
    );
  });

  it("names the variable and the offending value in the error", () => {
    let message = "";

    try {
      apiBaseUrl({ [API_URL_ENV]: "api.example.com" });
    } catch (error) {
      message = error instanceof Error ? error.message : String(error);
    }

    expect(message).toContain(API_URL_ENV);
    expect(message).toContain("api.example.com");
    expect(message).toContain(DEFAULT_API_BASE_URL);
  });

  // The production call sites pass nothing, so the default parameter is the
  // path that actually ships — cover it rather than only the injected one.
  it("reads process.env when no environment is passed", () => {
    vi.stubEnv(API_URL_ENV, "https://api.from-process-env.example.com");

    expect(apiBaseUrl()).toBe("https://api.from-process-env.example.com");
    expect(apiBaseUrlSource()).toBe("env");
  });

  it("uses the default when process.env has no value", () => {
    vi.stubEnv(API_URL_ENV, undefined);

    expect(apiBaseUrl()).toBe(DEFAULT_API_BASE_URL);
    expect(apiBaseUrlSource()).toBe("default");
  });
});
