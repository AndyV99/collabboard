/**
 * The credential-bearing endpoints, reachable only from server code.
 *
 * Four of the six either return or consume a refresh token. Keeping those out
 * of the shared catalogue in `lib/api/endpoints.ts` means the browser client has
 * no value it could pass to reach them, and `app/api/proxy` refuses the `auth`
 * prefix so it could not forward one if it did.
 *
 * `POST /auth/logout` is unauthenticated on the API by design — a client whose
 * access token has already expired still has to be able to log out, and the
 * refresh token is itself the credential.
 *
 * {@link createFirstOrganization} is the odd one out and is explained on its own
 * doc comment: it handles no token in either direction, and belongs here for the
 * adjacent reason that it takes a password.
 */

import type { ApiResult } from "@/lib/api/errors";
import { apiV1BaseUrl, sendRequest } from "@/lib/api/http";
import {
  type CreatedOrganization,
  type RegisteredUser,
  type SessionTokens,
  parseCreatedOrganization,
  parseEmpty,
  parseRegisteredUser,
  parseSessionTokens,
} from "@/lib/api/types";

/** `POST /auth/register`. Creates the account and its first organization. */
export function register(input: {
  email: string;
  password: string;
  displayName: string;
  organizationName?: string;
}): Promise<ApiResult<RegisteredUser>> {
  return sendRequest(
    {
      method: "POST",
      path: "/auth/register",
      body: {
        email: input.email,
        password: input.password,
        display_name: input.displayName,
        // Omitted rather than sent empty when absent: the API treats "" and
        // absent the same, but sending a key we do not mean is how a default
        // gets accidentally depended on.
        ...(input.organizationName === undefined
          ? {}
          : { organization_name: input.organizationName }),
      },
      parse: parseRegisteredUser,
    },
    { baseUrl: apiV1BaseUrl() },
  );
}

/** `POST /auth/login`. The only place a password crosses this app. */
export function login(input: {
  email: string;
  password: string;
}): Promise<ApiResult<SessionTokens>> {
  return sendRequest(
    {
      method: "POST",
      path: "/auth/login",
      body: { email: input.email, password: input.password },
      parse: parseSessionTokens,
    },
    { baseUrl: apiV1BaseUrl() },
  );
}

/**
 * `POST /organizations`. Gives an account stranded by a half-completed
 * registration the workspace it never got, with no operator involved.
 *
 * # Why it takes a password, and why that is not fixable here
 *
 * An account with zero memberships cannot hold a token of any kind: `Login`
 * refuses to issue one, `Issuer.Issue` refuses a nil tenant, and `Issuer.Verify`
 * refuses a zero `org` claim. The password is the only durable credential such
 * an account has. ADR 0009 records the two designs that were rejected for
 * getting a token into this state, so this is a structural shape rather than an
 * oversight to route around.
 *
 * # Why it is in this file and not in `lib/api/endpoints.ts`
 *
 * It breaks the rule stated in that module's comment — it neither returns nor
 * consumes a refresh token, so on a literal reading it belongs there. It is here
 * anyway, because the rule underneath that one is what matters: everything in
 * the shared catalogue is reachable from `lib/api/browser.ts`, which sends it to
 * `/api/proxy`. That proxy is a general forwarder which attaches a session's
 * bearer token server-side — and an account in this state has no session for it
 * to attach. Putting this endpoint in the catalogue would describe a
 * password-carrying request in the one place whose entries are, by design,
 * browser-reachable.
 *
 * `/api/proxy` would in fact refuse it today — `PROXIED_ROOTS` in
 * `lib/api/proxy-route.ts` is an allowlist, and `organizations` is not on it, so
 * the request would come back `not_allowed`. That is a reason this is safe
 * rather than a reason it belongs there: the allowlist is what stops a
 * *forgotten* entry leaking, not an argument for adding one that would have to
 * be deliberately excluded. Server-only is the property worth keeping; the
 * refresh token was only ever the most common reason for it.
 *
 * No `accessToken` is passed, deliberately: the route is on the API's
 * unauthenticated group and there is nothing to pass.
 *
 * Statuses worth knowing at the call site: 401 for a wrong password *and* for an
 * unknown address (the same `verifyCredential` login uses, so the same
 * anti-enumeration property holds), 409 when the account already has an
 * organization, and 429 out of the *login* budget, which this route shares and
 * charges before it checks the credential.
 */
export function createFirstOrganization(input: {
  email: string;
  password: string;
  organizationName?: string;
}): Promise<ApiResult<CreatedOrganization>> {
  return sendRequest(
    {
      method: "POST",
      path: "/organizations",
      body: {
        email: input.email,
        password: input.password,
        // Omitted rather than sent empty when absent, for the reason `register`
        // gives above: both callers reach the same `workspaceName` on the API,
        // and sending a key we do not mean is how a default gets accidentally
        // depended on.
        ...(input.organizationName === undefined
          ? {}
          : { organization_name: input.organizationName }),
      },
      parse: parseCreatedOrganization,
    },
    { baseUrl: apiV1BaseUrl() },
  );
}

/** `POST /auth/logout`. 204 for an unknown token, which is not an error. */
export function logout(refreshToken: string): Promise<ApiResult<null>> {
  return sendRequest(
    {
      method: "POST",
      path: "/auth/logout",
      body: { refresh_token: refreshToken },
      parse: parseEmpty,
      expectNoContent: true,
    },
    { baseUrl: apiV1BaseUrl() },
  );
}

/**
 * `POST /auth/organization`. Authenticated, and the only endpoint that takes an
 * organization id from a client — the API re-checks membership and answers 403
 * for a non-member, so a `forbidden` here is the expected failure, not a bug.
 *
 * It returns a whole new session (a new session id and a new refresh token), so
 * its caller must write all three cookies, not just the access token.
 */
export function switchOrganization(
  accessToken: string,
  organizationId: string,
): Promise<ApiResult<SessionTokens>> {
  return sendRequest(
    {
      method: "POST",
      path: "/auth/organization",
      body: { organization_id: organizationId },
      parse: parseSessionTokens,
    },
    { baseUrl: apiV1BaseUrl(), accessToken },
  );
}
