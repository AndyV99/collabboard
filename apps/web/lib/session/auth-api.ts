/**
 * The five auth endpoints, reachable only from server code.
 *
 * These live here rather than in `lib/api/endpoints.ts` because every one of
 * them either returns or consumes a refresh token. Keeping them out of the
 * shared endpoint catalogue means the browser client has no value it could pass
 * to reach them, and `app/api/proxy` refuses the `auth` prefix so it could not
 * forward one if it did.
 *
 * `POST /auth/logout` is unauthenticated on the API by design — a client whose
 * access token has already expired still has to be able to log out, and the
 * refresh token is itself the credential.
 */

import type { ApiResult } from "@/lib/api/errors";
import { apiV1BaseUrl, sendRequest } from "@/lib/api/http";
import {
  type RegisteredUser,
  type SessionTokens,
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
