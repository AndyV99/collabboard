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
  ORGANIZATION_EXISTS,
  REFUSAL_CROSS_ORIGIN,
  REFUSAL_HEADER,
  UNCONFIRMED,
  describeFirstOrganizationFailure,
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
  });

  it("no longer sends the user to support, because #34 gave them a way out", () => {
    // The copy is pinned here precisely so that removing this dead end is a
    // deliberate edit. `POST /api/v1/organizations` does what the support
    // ticket used to ask for, with no operator involved, so the sentence names
    // the affordance instead.
    expect(NO_ORGANIZATION).not.toMatch(/contact support/i);
    expect(NO_ORGANIZATION).not.toMatch(/support/i);
    expect(NO_ORGANIZATION).toMatch(/create the missing workspace/i);
  });

  it("reads a 403 marked by our own CSRF guard as a refusal, not a missing workspace", () => {
    const failure = describeLoginFailure(403, "This request did not come from this site.", refused);

    expect(failure.kind).toBe("blocked");
    expect(failure.message).not.toBe(NO_ORGANIZATION);
  });

  it("keeps the two 403s apart on kind and on copy, which is the whole point", () => {
    // Load-bearing. The status alone cannot distinguish "this account has no
    // workspace" from "this app refused a cross-origin post", and the only
    // signal is the private header — matching on message text instead would
    // make the copy above load-bearing, which is what REFUSAL_HEADER avoids.
    const bare = describeLoginFailure(403, "this account does not belong to an organization", none);
    const guarded = describeLoginFailure(403, "This request did not come from this site.", refused);

    expect(bare.kind).toBe("no_organization");
    expect(guarded.kind).toBe("blocked");
    expect(bare.kind).not.toBe(guarded.kind);
    expect(bare.message).not.toBe(guarded.message);
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

describe("creating the workspace a stranded account never got", () => {
  it("treats a 409 as the situation having resolved itself, not as an error", () => {
    // The account already has an organization: another tab, or two clicks that
    // raced. Nothing is wrong and nothing needs creating — the next step is the
    // sign-in the user was trying to do in the first place.
    const failure = describeFirstOrganizationFailure(409, "this account already belongs to an organization", none);

    expect(failure.kind).toBe("organization_exists");
    expect(failure.message).toBe(ORGANIZATION_EXISTS);
    expect(failure.message).toMatch(/sign in/i);
    expect(failure.message).not.toMatch(/error|failed|went wrong|try again/i);
  });

  it("surfaces a 429 with its wait, because this shares the sign-in budget", () => {
    // The route is charged against the same per-account and per-address
    // counters as login, *before* the credential is checked. So a user who has
    // just failed a few sign-ins can be refused on their first click here, and
    // "something went wrong" would be a much worse answer than "wait 15
    // minutes" when the real answer is the second one.
    const failure = describeFirstOrganizationFailure(429, "too many attempts, try again later", withRetry("900"));

    expect(failure.kind).toBe("rate_limited");
    expect(failure.retryAfterSeconds).toBe(900);
    expect(failure.message).toMatch(/about 15 minutes/);
    expect(failure.message).toMatch(/sign-in/i);
  });

  it("still explains a 429 that arrived without a Retry-After", () => {
    const failure = describeFirstOrganizationFailure(429, null, none);

    expect(failure.kind).toBe("rate_limited");
    expect(failure.retryAfterSeconds).toBeUndefined();
    expect(failure.message).toMatch(/too many/i);
  });

  it("says exactly what login says for a 401, and blames no field", () => {
    // Reachable only when the password was edited after the 403. The endpoint
    // shares `verifyCredential` with login, so an unknown address and a wrong
    // password are the same answer here too.
    const failure = describeFirstOrganizationFailure(401, "invalid email or password", none);

    expect(failure.kind).toBe("invalid_credentials");
    expect(failure.message).toBe("Email or password is incorrect.");
    expect(failure.message).not.toMatch(/no account|not found|unknown|does not exist/i);
  });

  it("never reads a bare 403 as a missing workspace, unlike login", () => {
    // `CreateFirstOrganization` cannot return `ErrNoOrganization` — that state
    // is the precondition of the call, not one of its answers. So an unmarked
    // 403 on this route is something we do not understand, and claiming it
    // means "no workspace" would offer the user a button that just failed.
    const guarded = describeFirstOrganizationFailure(403, "This request did not come from this site.", refused);
    const bare = describeFirstOrganizationFailure(403, "…", none);

    expect(guarded.kind).toBe("blocked");
    expect(bare.kind).toBe("unavailable");
    expect(bare.kind).not.toBe("no_organization");
    expect(bare.message).not.toBe(NO_ORGANIZATION);
  });

  it("keeps a 400's message and falls back when the body was not the envelope", () => {
    expect(describeFirstOrganizationFailure(400, "Workspace name must be text.", none)).toEqual({
      kind: "invalid_input",
      message: "Workspace name must be text.",
    });
    expect(describeFirstOrganizationFailure(400, null, none).message).toMatch(/check the details/i);
  });

  it("does not claim to know what a 5xx did", () => {
    for (const status of [500, 502, 503]) {
      expect(describeFirstOrganizationFailure(status, null, none).kind).toBe("unavailable");
    }
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
