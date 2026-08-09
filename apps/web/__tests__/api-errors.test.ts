import { describe, expect, it } from "vitest";

import {
  errorFromStatus,
  kindForStatus,
  parseErrorBody,
  parseRetryAfter,
} from "@/lib/api/errors";

describe("kindForStatus", () => {
  // The statuses apps/api actually produces. If the API grows a new one, this
  // list is where the client learns about it.
  it.each([
    [400, "bad_request"],
    [401, "unauthorized"],
    [403, "forbidden"],
    [404, "not_found"],
    [409, "conflict"],
    [429, "rate_limited"],
    [500, "server_error"],
    [502, "server_error"],
    [503, "server_error"],
  ] as const)("maps %i to %s", (status, kind) => {
    expect(kindForStatus(status)).toBe(kind);
  });

  it("does not guess at a status it has no mapping for", () => {
    expect(kindForStatus(418)).toBe("unexpected_status");
    expect(kindForStatus(302)).toBe("unexpected_status");
  });
});

describe("parseErrorBody", () => {
  it("reads the API's one error shape", () => {
    expect(parseErrorBody({ error: "card not found" })).toBe("card not found");
  });

  it.each([
    ["an HTML error page", "<html>502 Bad Gateway</html>"],
    ["a body with no error field", { message: "nope" }],
    ["an empty error field", { error: "  " }],
    ["an array", [{ error: "x" }]],
    ["null", null],
  ])("returns null for %s", (_name, body) => {
    expect(parseErrorBody(body)).toBeNull();
  });
});

describe("parseRetryAfter", () => {
  it("reads the delta-seconds form the API sends", () => {
    expect(parseRetryAfter("42")).toBe(42);
  });

  it("ignores the HTTP-date form rather than half-parsing it", () => {
    expect(parseRetryAfter("Wed, 21 Oct 2026 07:28:00 GMT")).toBeUndefined();
  });

  it("ignores an absent or negative value", () => {
    expect(parseRetryAfter(null)).toBeUndefined();
    expect(parseRetryAfter("-1")).toBeUndefined();
  });
});

describe("errorFromStatus", () => {
  it("prefers the API's message over the default", () => {
    const error = errorFromStatus(409, { error: "email is already registered" });

    expect(error).toEqual({
      kind: "conflict",
      status: 409,
      message: "email is already registered",
    });
  });

  it("falls back to a safe default when the body is unusable", () => {
    const error = errorFromStatus(500, "<html>oops</html>");

    expect(error.kind).toBe("server_error");
    expect(error.message).not.toContain("html");
  });

  it("carries Retry-After on a 429 and nowhere else", () => {
    expect(errorFromStatus(429, { error: "too many attempts" }, "30")).toEqual({
      kind: "rate_limited",
      status: 429,
      message: "too many attempts",
      retryAfterSeconds: 30,
    });

    expect(errorFromStatus(403, { error: "nope" }, "30")).not.toHaveProperty(
      "retryAfterSeconds",
    );
  });
});
