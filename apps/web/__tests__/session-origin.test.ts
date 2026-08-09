import { describe, expect, it } from "vitest";

import { checkSameOrigin } from "@/lib/session/origin";
import { PROXIED_ROOTS, proxyTarget } from "@/lib/api/proxy-route";

const ORIGIN = "https://collabboard.example";

function headers(values: Record<string, string>): Headers {
  return new Headers(values);
}

describe("checkSameOrigin", () => {
  it("allows a same-origin fetch", () => {
    expect(checkSameOrigin(headers({ "sec-fetch-site": "same-origin" }), ORIGIN)).toEqual({
      allowed: true,
    });
  });

  it("allows a typed-in URL, which sends Sec-Fetch-Site: none", () => {
    expect(checkSameOrigin(headers({ "sec-fetch-site": "none" }), ORIGIN).allowed).toBe(
      true,
    );
  });

  it("refuses a cross-site request", () => {
    expect(checkSameOrigin(headers({ "sec-fetch-site": "cross-site" }), ORIGIN)).toEqual({
      allowed: false,
      reason: "cross_site",
    });
  });

  it("refuses a same-site request from a sibling subdomain", () => {
    // A sibling subdomain is a different trust boundary, and this app has no
    // reason to accept writes from one.
    expect(checkSameOrigin(headers({ "sec-fetch-site": "same-site" }), ORIGIN)).toEqual({
      allowed: false,
      reason: "cross_site",
    });
  });

  it("falls back to Origin when Sec-Fetch-Site is absent", () => {
    expect(checkSameOrigin(headers({ origin: ORIGIN }), ORIGIN).allowed).toBe(true);
    expect(checkSameOrigin(headers({ origin: "https://evil.example" }), ORIGIN)).toEqual({
      allowed: false,
      reason: "origin_mismatch",
    });
  });

  it("refuses a request with no origin signal at all", () => {
    // Not a browser. Non-browser clients should hold a bearer token and talk to
    // the Go API, where there are no cookies and so no CSRF.
    expect(checkSameOrigin(headers({}), ORIGIN)).toEqual({
      allowed: false,
      reason: "no_origin_signal",
    });
  });

  it("prefers Sec-Fetch-Site over a forged Origin", () => {
    expect(
      checkSameOrigin(
        headers({ "sec-fetch-site": "cross-site", origin: ORIGIN }),
        ORIGIN,
      ).allowed,
    ).toBe(false);
  });
});

describe("proxyTarget", () => {
  it("allows the resource roots the UI needs", () => {
    expect(proxyTarget(["projects"]).allowed).toBe(true);
    expect(proxyTarget(["boards", "b1", "cards"])).toEqual({
      allowed: true,
      path: "/boards/b1/cards",
    });
  });

  it("does not include auth or ws", () => {
    // The single most important entry not on the list: `POST /api/proxy/auth/login`
    // would hand a browser a refresh token in a response body.
    expect(PROXIED_ROOTS.has("auth")).toBe(false);
    expect(PROXIED_ROOTS.has("ws")).toBe(false);
    expect(proxyTarget(["auth", "login"])).toEqual({
      allowed: false,
      reason: "not_allowed",
    });
  });

  it("re-encodes segments so one cannot reshape the URL", () => {
    // Next hands these over already percent-decoded.
    expect(proxyTarget(["cards", "../../auth/login"])).toEqual({
      allowed: true,
      path: "/cards/..%2F..%2Fauth%2Flogin",
    });
  });

  it("keeps a query string for future list filters", () => {
    expect(proxyTarget(["projects"], "?archived=true")).toEqual({
      allowed: true,
      path: "/projects?archived=true",
    });
  });

  it("refuses an empty path", () => {
    expect(proxyTarget([])).toEqual({ allowed: false, reason: "empty_path" });
    expect(proxyTarget([""])).toEqual({ allowed: false, reason: "empty_path" });
  });
});
