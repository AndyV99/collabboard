/**
 * What each status turns into on screen.
 *
 * The enumeration tests here are the ones that matter. `apps/api` spends a
 * matching argon2id derivation on an unknown address so that timing does not
 * distinguish it from a wrong password, and answers both with the same body;
 * this file asserts that the web layer keeps that promise in words.
 */

import { describe, expect, it } from "vitest";

import {
  NO_ORGANIZATION,
  REFUSAL_CROSS_ORIGIN,
  REFUSAL_HEADER,
  UNCONFIRMED,
  describeLoginFailure,
  describeRegistrationFailure,
  describeWait,
  networkFailure,
  readErrorMessage,
  retryAfterSeconds,
} from "@/lib/auth/outcomes";

const none = new Headers();

const withRetry = (seconds: string) => new Headers({ "retry-after": seconds });

const refused = new Headers({ [REFUSAL_HEADER]: REFUSAL_CROSS_ORIGIN });

describe("sign-in discloses nothing about an address", () => {
  it("says the same thing for an unknown address and a wrong password", () => {
    // Both are 401 with the same body, so there is only one input to map. The
    // test is that the mapping does not add a distinction from somewhere else.
    const unknown = describeLoginFailure(401, "invalid email or password", none);
    const wrong = describeLoginFailure(401, "invalid email or password", none);

    expect(unknown).toEqual(wrong);
    expect(unknown.kind).toBe("invalid_credentials");
  });

  it("does not blame a field, and does not repeat the API's wording either", () => {
    const failure = describeLoginFailure(401, "invalid email or password", none);

    // The relayed message is discarded for 401 on purpose: the copy shown on
    // the sign-in screen should not change because a server-side string did.
    expect(failure.message).toBe("Email or password is incorrect.");
    expect(failure.message).not.toMatch(
      /no account|not found|unknown|does not exist|unregistered|no user/i,
    );
  });

  it("keeps a 400's message, because that describes the request", () => {
    expect(describeLoginFailure(400, "Email and password are required.", none).message).toBe(
      "Email and password are required.",
    );
  });

  it("falls back to its own copy when the body was not the envelope", () => {
    expect(describeLoginFailure(400, null, none).kind).toBe("invalid_input");
    expect(describeLoginFailure(400, null, none).message).toMatch(/check the details/i);
  });
});

describe("403 means two completely different things", () => {
  it("reads a bare 403 as the account having no workspace", () => {
    const failure = describeLoginFailure(403, "this account does not belong to an organization", none);

    expect(failure.kind).toBe("no_organization");
    expect(failure.message).toBe(NO_ORGANIZATION);
  });

  it("tells the user not to sign up again", () => {
    // The harm in this state is a user who re-registers and collects a 409.
    expect(NO_ORGANIZATION).toMatch(/signing up again .* will not fix it/i);
    expect(NO_ORGANIZATION).toMatch(/contact support/i);
  });

  it("reads a 403 marked by our own CSRF guard as a refusal, not a missing workspace", () => {
    const failure = describeLoginFailure(403, "This request did not come from this site.", refused);

    expect(failure.kind).toBe("blocked");
    expect(failure.message).not.toBe(NO_ORGANIZATION);
  });
});

describe("rate limiting", () => {
  it("carries Retry-After through so the form can respect it", () => {
    const failure = describeLoginFailure(429, "too many attempts, try again later", withRetry("900"));

    expect(failure.kind).toBe("rate_limited");
    expect(failure.retryAfterSeconds).toBe(900);
    expect(failure.message).toMatch(/about 15 minutes/);
  });

  it("still explains itself when no Retry-After arrived", () => {
    const failure = describeLoginFailure(429, null, none);

    expect(failure.retryAfterSeconds).toBeUndefined();
    expect(failure.message).toMatch(/too many/i);
  });

  it("parses only the delta-seconds form the API sends", () => {
    expect(retryAfterSeconds("30")).toBe(30);
    expect(retryAfterSeconds(" 30 ")).toBe(30);
    expect(retryAfterSeconds("0")).toBe(0);
    expect(retryAfterSeconds("-1")).toBeUndefined();
    expect(retryAfterSeconds("Wed, 21 Oct 2015 07:28:00 GMT")).toBeUndefined();
    expect(retryAfterSeconds(null)).toBeUndefined();
  });

  it("rounds a wait to something a person would say", () => {
    expect(describeWait(1)).toBe("in about 1 second");
    expect(describeWait(45)).toBe("in about 45 seconds");
    expect(describeWait(60)).toBe("in about 1 minute");
    expect(describeWait(900)).toBe("in about 15 minutes");
    expect(describeWait(undefined)).toBe("in a moment");
  });
});

describe("registration", () => {
  it("relays the deliberate 409 plainly", () => {
    const failure = describeRegistrationFailure(409, "email is already registered", none);

    expect(failure.kind).toBe("email_taken");
    expect(failure.message).toMatch(/already exists/i);
  });

  it("treats every 5xx as 'we cannot tell whether an account exists'", () => {
    // 500 is the API's own failure — including the one that leaves an account
    // with no organization — and 502 is what `relayStatus` produces for a
    // request that never got an answer. Neither says whether a user row was
    // committed, so neither claims to.
    for (const status of [500, 502, 503]) {
      const failure = describeRegistrationFailure(status, null, none);

      expect(failure.kind).toBe("unconfirmed");
      expect(failure.message).toBe(UNCONFIRMED);
    }
  });

  it("tells the user to sign in rather than sign up again", () => {
    expect(UNCONFIRMED).toMatch(/try signing in/i);
    expect(UNCONFIRMED).toMatch(/do not sign up again/i);
  });

  it("does not confuse a CSRF refusal with anything else", () => {
    expect(describeRegistrationFailure(403, "…", refused).kind).toBe("blocked");
    expect(describeRegistrationFailure(403, "…", none).kind).toBe("unavailable");
  });
});

describe("reading the error envelope", () => {
  it("takes the API's error string", async () => {
    const response = new Response(JSON.stringify({ error: "  nope  " }), { status: 400 });

    expect(await readErrorMessage(response)).toBe("nope");
  });

  it("returns null for anything that is not the envelope", async () => {
    expect(await readErrorMessage(new Response("<html>502</html>", { status: 502 }))).toBeNull();
    expect(await readErrorMessage(new Response("[]", { status: 400 }))).toBeNull();
    expect(await readErrorMessage(new Response('{"error":""}', { status: 400 }))).toBeNull();
    expect(await readErrorMessage(new Response('{"detail":"x"}', { status: 400 }))).toBeNull();
  });
});

describe("a rejected fetch", () => {
  it("is the browser being offline, not a credential problem", () => {
    expect(networkFailure().kind).toBe("unavailable");
    expect(networkFailure().message).not.toMatch(/password|account/i);
  });
});
