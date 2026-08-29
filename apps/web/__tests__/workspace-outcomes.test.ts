/**
 * The copy, as a unit test rather than as an outage someone has to reproduce.
 *
 * Two properties are asserted repeatedly and they are the ones that matter:
 * a 404 never claims to know whether the thing exists elsewhere, and "try
 * again" is never offered for a failure that trying again cannot fix.
 */

import { describe, expect, it } from "vitest";

import type { ApiError, ApiErrorKind } from "@/lib/api/errors";
import {
  describeAddMemberFailure,
  describeLoadFailure,
  describeWriteFailure,
} from "@/lib/workspace/outcomes";

function error(kind: ApiErrorKind, extra: Partial<ApiError> = {}): ApiError {
  return { kind, message: "from the API", status: 500, ...extra };
}

describe("describeLoadFailure", () => {
  it("names the subject it could not load", () => {
    expect(describeLoadFailure(error("network"), "projects").title).toContain("projects");
    expect(describeLoadFailure(error("network"), "boards").title).toContain("boards");
  });

  it.each<[ApiErrorKind, boolean]>([
    ["network", true],
    ["server_error", true],
    ["malformed", true],
    ["rate_limited", true],
    ["unauthorized", false],
    ["forbidden", false],
    ["not_found", false],
    // A malformed id in the URL. Retrying produces the identical 400 forever,
    // so offering the button is a loop with no exit.
    ["bad_request", false],
  ])("offers a retry for %s: %s", (kind, retryable) => {
    expect(describeLoadFailure(error(kind), "projects").retryable).toBe(retryable);
  });

  // #82. `pathUUID` in crud.go answers 400 for a malformed id before any lookup
  // happens, so every workspace screen can produce one from a hand-edited or
  // truncated URL -- a link that lost its last characters in a chat client, an
  // id pasted with a stray space. The default branch used to catch it and tell
  // those users the server was broken, with a retry button that could never
  // work.
  it("treats a malformed id as an address that names nothing, not a server fault", () => {
    const failure = describeLoadFailure(error("bad_request"), "boards");

    expect(failure.retryable).toBe(false);
    expect(failure.message).not.toMatch(/our side|went wrong|try again/i);

    // Same copy as not_found, because from the reader's side it is the same
    // situation and the screen cannot show the difference.
    expect(failure).toEqual(describeLoadFailure(error("not_found"), "boards"));
  });

  // describeWriteFailure relays the API's 400 verbatim and is right to -- that
  // message is about what the user just typed. This one must not: the API says
  // `board_id must be a uuid`, which names a path segment nobody typed as a
  // field, and putting it on a page about a missing board leaks an internal
  // parameter name for no benefit.
  it("does not relay the API's message for a malformed id", () => {
    const failure = describeLoadFailure(
      error("bad_request", { message: "board_id must be a uuid" }),
      "boards",
    );

    expect(failure.message).not.toContain("board_id");
    expect(failure.message).not.toContain("uuid");
  });

  it("does not send an expired session to /login, which would bounce it back", () => {
    // The cookies are still on the browser, so /login redirects anyone holding
    // them straight back here and the page fails identically. Sign out is the
    // only control that clears them, and it is in the header.
    const failure = describeLoadFailure(error("unauthorized"), "projects");

    expect(failure.message).toContain("Sign out");
    expect(failure.message).not.toContain("/login");
  });

  it("treats a 403 as a revoked membership, which is recoverable", () => {
    // The realistic cause is a membership withdrawn while the session was open,
    // and signing in again re-derives the organizations the account is in. Copy
    // that read "you do not have access" would leave the user on a dead end.
    // #75 makes GET /me answer 403 for exactly this state.
    const failure = describeLoadFailure(error("forbidden"), "projects");

    expect(failure.message).toContain("sign in again");
    expect(failure.retryable).toBe(false);
  });

  it("keeps 404 ambiguous between 'gone' and 'another workspace'", () => {
    // `crud.go` answers 404 for another tenant's object on purpose. Copy that
    // implied "it exists but is not yours" would hand back the bit the API
    // withheld.
    const failure = describeLoadFailure(error("not_found"), "boards");

    expect(failure.message).toContain("do not exist");
    expect(failure.message).toContain("workspace you are not a member of");
  });

  it("relays the wait from Retry-After", () => {
    const failure = describeLoadFailure(
      error("rate_limited", { status: 429, retryAfterSeconds: 30 }),
      "projects",
    );

    expect(failure.message).toContain("30 seconds");
  });
});

describe("describeWriteFailure", () => {
  it("relays the API's own message for a 400, and only for a 400", () => {
    // A 400 describes the submission, so it is more precise than anything this
    // module could write. Everything else is local copy, so a server-side
    // string changing cannot change a promise a screen makes.
    expect(describeWriteFailure(error("bad_request", { message: "name is too long" }), "x"))
      .toBe("name is too long");

    expect(describeWriteFailure(error("server_error", { message: "boom" }), "x"))
      .not.toContain("boom");
  });

  it("says what could not be done, in the caller's words", () => {
    expect(describeWriteFailure(error("forbidden"), "archive this project"))
      .toContain("archive this project");
  });

  it("says nothing was saved, and that a 403 may be recoverable", () => {
    const message = describeWriteFailure(error("forbidden"), "rename this project");

    expect(message).toContain("nothing was saved");
    expect(message).toContain("sign out and back in");
  });

  it("does not claim a network failure left nothing behind", () => {
    // The request may have been applied and the answer lost. Telling the user
    // to retry blind is how a project gets created twice.
    const message = describeWriteFailure(error("network"), "create the project");

    expect(message).toContain("not clear whether");
    expect(message).toContain("Reload");
  });

  it("treats a 404 as 'gone or not yours', without guessing which", () => {
    expect(describeWriteFailure(error("not_found"), "rename this project"))
      .toContain("workspace you are not a member of");
  });
});

describe("describeAddMemberFailure", () => {
  it("explains that a 404 means no account, not a typo we can fix", () => {
    // This endpoint never creates a user (ADR 0008), so the only way forward is
    // for that person to sign up. Copy that said "check the address" would send
    // the user round a loop they cannot exit.
    const failure = describeAddMemberFailure(error("not_found", { status: 404 }));

    expect(failure.kind).toBe("no_account");
    expect(failure.message).toContain("sign up");
  });

  it("points a 409 at the list already on the page", () => {
    const failure = describeAddMemberFailure(error("conflict", { status: 409 }));

    expect(failure.kind).toBe("already_a_member");
    expect(failure.message).toContain("already in this workspace");
  });

  it("treats a 403 as a role that changed, not something to retry", () => {
    const failure = describeAddMemberFailure(error("forbidden", { status: 403 }));

    expect(failure.kind).toBe("not_permitted");
    expect(failure.message).toContain("reload");
  });

  it("relays a 400 verbatim, which is where the role rejection lands", () => {
    // "role must be \"member\" or \"admin\"; ownership is not transferable
    // through this endpoint" is the API's sentence and it is a good one.
    const failure = describeAddMemberFailure(
      error("bad_request", { status: 400, message: "role must be \"member\" or \"admin\"" }),
    );

    expect(failure.kind).toBe("invalid_input");
    expect(failure.message).toBe("role must be \"member\" or \"admin\"");
  });

  it("says nobody was added when the session ended", () => {
    expect(describeAddMemberFailure(error("unauthorized", { status: 401 })).message)
      .toContain("nobody was added");
  });
});
