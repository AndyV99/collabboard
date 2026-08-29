/**
 * What the workspace screens say when something goes wrong.
 *
 * Pure functions over an {@link ApiError}, so every branch of the copy is a unit
 * test rather than an outage someone has to reproduce. The screens import these
 * instead of writing sentences inline, which is what keeps "a 404 means it does
 * not exist *or* it is not yours" from being restated three different ways on
 * three different pages.
 *
 * # Two families, because the recovery differs
 *
 * {@link describeLoadFailure} is for a list that did not load. The user did not
 * ask for anything; a screen is simply missing, and the recovery is "try again"
 * or nothing at all.
 *
 * {@link describeWriteFailure} and {@link describeAddMemberFailure} are for
 * something the user just submitted. The recovery is a decision they have to
 * make about the thing they typed, so the copy has to say what to do with it.
 *
 * # The API's message is relayed for 400 and for nothing else
 *
 * `apps/api` writes its 400s about the request — "name is too long", "at least
 * one of name or description is required" — and those are more precise than
 * anything this file could invent, so they are shown verbatim. Every other
 * status gets copy from here. That is the same split `lib/auth/outcomes.ts`
 * makes, and for the same reason: a server-side string changing must not
 * silently change the promise a screen makes.
 *
 * # 404 never means "it exists but is not yours"
 *
 * `crud.go`'s `notFound` answers 404 for another tenant's object deliberately —
 * a 403 there would confirm the id is real, which is an existence oracle across
 * the tenant boundary. So no message below distinguishes the two, because the
 * screen genuinely cannot, and writing copy that implies otherwise would leak
 * the bit the API went to trouble to withhold.
 */

import type { ApiError } from "@/lib/api/errors";
import { describeWait } from "@/lib/auth/outcomes";

/** A list that did not load, as the screen renders it. */
export type LoadFailure = {
  title: string;
  message: string;
  /** Whether trying the same request again could plausibly work. */
  retryable: boolean;
};

/**
 * What to show in place of a list.
 *
 * `subject` is the plural noun the sentence is about — "projects", "boards",
 * "members" — so one function serves every list without the copy reading as
 * though it were written for a different one.
 */
export function describeLoadFailure(error: ApiError, subject: string): LoadFailure {
  switch (error.kind) {
    case "unauthorized":
      return {
        // `serverApi` cannot refresh and `proxy.ts` already had its chance
        // before this render, so a 401 here is a session that ended between the
        // two — revoked elsewhere, usually. It does not offer a link to sign in:
        // the cookies are still on the browser, `/login` bounces anyone who has
        // them back here, and the page would fail identically. Sign out is the
        // one control that actually clears them, and it is in the header.
        title: "Your session is no longer valid",
        message:
          "The workspace could not be read because this session has ended. Use Sign out at the top of the page, then sign in again.",
        retryable: false,
      };

    case "forbidden":
      // The realistic cause is a membership that was revoked while the session
      // was open: the token still names the organization, the database no
      // longer agrees, and the API says so. That is recoverable — signing in
      // again re-derives the organizations this account actually belongs to —
      // so the copy gives the instruction rather than presenting a dead end.
      //
      // #75 makes this reachable in one more place: `GET /me` currently answers
      // 200 with a zero-valued organization for a revoked membership and will
      // answer 403 instead. Nothing in this app calls `/me` today, but the
      // sentence has to be right before something does.
      return {
        title: "You are no longer a member of this workspace",
        message: `Your access to these ${subject} was withdrawn, or this session is for an organization your account has been removed from. Use Sign out at the top of the page, then sign in again to see the workspaces you are still in.`,
        retryable: false,
      };

    case "not_found":
    // A malformed id in the URL, which `crud.go`'s `pathUUID` answers 400 for
    // before any lookup happens. It shares `not_found`'s copy deliberately.
    //
    // From the reader's side the two are the same situation: this address does
    // not name anything. The distinction between "well-formed and absent" and
    // "not even an id" is real to the API and invisible to a person who
    // arrived here from a link that lost its last characters in a chat client,
    // or an id pasted with a stray space — which is the common way somebody
    // reaches a broken address. Inventing a third state for it would mean
    // writing copy about a difference the screen cannot show.
    //
    // What it must not do is what the `default` branch did: claim a server
    // fault and offer a retry. Nothing went wrong on the server, and retrying
    // produces the identical 400 forever — a loop with no exit.
    //
    // The API's own message is NOT relayed here, unlike in describeWriteFailure.
    // `board_id must be a uuid` is about a path segment the user never typed as
    // a field, and showing it would leak an internal parameter name onto a page
    // about a missing board.
    case "bad_request":
      return {
        title: "Not found",
        message: `These ${subject} do not exist, or they belong to a workspace you are not a member of.`,
        retryable: false,
      };

    case "rate_limited":
      return {
        title: "Too many requests",
        message: `The server asked us to slow down. Try again ${describeWait(error.retryAfterSeconds)}.`,
        retryable: true,
      };

    case "network":
      return {
        title: `Could not load ${subject}`,
        message:
          "The server did not answer. Check your connection, then try again — nothing has been lost.",
        retryable: true,
      };

    default:
      // server_error, malformed, unexpected_status, and `conflict` — which
      // genuinely should not reach a GET. `bad_request` used to be listed here
      // as equally unreachable and was not: a malformed id in the URL produces
      // one on every workspace screen, and this branch told those users the
      // server was broken and handed them a retry button. It has its own case
      // above now.
      //
      // What remains here all means the same thing to someone looking at an
      // empty page: this is not your fault and there is nothing to fix here.
      return {
        title: `Could not load ${subject}`,
        message:
          "Something went wrong on our side. Try again in a moment — nothing has been lost.",
        retryable: true,
      };
  }
}

/**
 * What to show under a form whose submission failed.
 *
 * `subject` names the thing that was being written, in the sentence's own words
 * — "create the project", "rename this project", "archive this project".
 */
export function describeWriteFailure(error: ApiError, subject: string): string {
  switch (error.kind) {
    case "bad_request":
      // The API described what it rejected about the submission. It is about
      // the request rather than about stored state, so it is safe and it is
      // better than anything written here.
      return error.message;

    case "unauthorized":
      return "Your session has ended, so nothing was saved. Use Sign out at the top of the page, then sign in again.";

    case "forbidden":
      // Same reasoning as the load path: "not allowed" reads as permanent, and
      // the usual cause — a membership revoked or a role changed since the page
      // rendered — is not. Nothing was saved either way, which is the part the
      // user most needs to know.
      return `Your account is not allowed to ${subject}, so nothing was saved. If your role or your membership changed while this page was open, reload it — or sign out and back in.`;

    case "not_found":
      return "That no longer exists, or it belongs to a workspace you are not a member of. Reload the page.";

    case "conflict":
      return "This changed while you were working on it. Reload the page and try again.";

    case "rate_limited":
      return `Too many attempts. Try again ${describeWait(error.retryAfterSeconds)}.`;

    case "network":
      return `The server could not be reached, so it is not clear whether the change was saved. Reload the page before trying to ${subject} again.`;

    default:
      return `Something went wrong and we could not ${subject}. Try again in a moment.`;
  }
}

/** What went wrong adding a member, as a closed set the form branches on. */
export type AddMemberFailureKind =
  | "invalid_input"
  | "not_permitted"
  | "no_account"
  | "already_a_member"
  | "rate_limited"
  | "unavailable";

export type AddMemberFailure = {
  kind: AddMemberFailureKind;
  message: string;
};

/**
 * Maps a failed `POST /members`.
 *
 * The three interesting statuses each need copy that says what to do next, and
 * none of them is a retry:
 *
 * - **404** — the address has no account here. ADR 0008 is explicit that this
 *   endpoint never creates one, so the answer is "they have to sign up first",
 *   and the copy says exactly that rather than implying a typo. It is also the
 *   one bit this operation cannot avoid disclosing, which is why the API's own
 *   sentence carries nothing else and why this one does not either.
 * - **409** — already a member. Not a failure worth alarm: they are in the list
 *   on this very page, so the message points at it.
 * - **403** — the caller's role does not permit it. The screen should not have
 *   offered the form at all (see `lib/workspace/roles.ts`), so reaching this
 *   means the role changed underneath them, and the copy says so instead of
 *   suggesting they try again.
 */
export function describeAddMemberFailure(error: ApiError): AddMemberFailure {
  switch (error.kind) {
    case "bad_request":
      return { kind: "invalid_input", message: error.message };

    case "forbidden":
      return {
        kind: "not_permitted",
        message:
          "Your role in this workspace does not allow adding people. If it changed while this page was open, reload to see the current one.",
      };

    case "not_found":
      return {
        kind: "no_account",
        message:
          "Nobody has signed up with that address yet. They need a CollabBoard account before they can be added to this workspace — ask them to sign up, then add them.",
      };

    case "conflict":
      return {
        kind: "already_a_member",
        message: "That person is already in this workspace — they are in the list below.",
      };

    case "rate_limited":
      return {
        kind: "rate_limited",
        message: `Too many attempts. Try again ${describeWait(error.retryAfterSeconds)}.`,
      };

    case "unauthorized":
      return {
        kind: "unavailable",
        message:
          "Your session has ended, so nobody was added. Use Sign out at the top of the page, then sign in again.",
      };

    default:
      return {
        kind: "unavailable",
        message: "Something went wrong and nobody was added. Try again in a moment.",
      };
  }
}
