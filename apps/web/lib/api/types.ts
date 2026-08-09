/**
 * The API's payload types, and a parser for each.
 *
 * # Why these are hand-written rather than generated
 *
 * `apps/api` publishes no OpenAPI document. Generating a client would mean
 * first hand-writing a spec that mirrors the Go structs, then generating from
 * it — two artefacts to keep in step with the handlers instead of one, and the
 * spec would drift silently because nothing type-checks it against the Go code.
 *
 * The second reason matters more. A generated client's types are a *cast*: the
 * response body is asserted to match and nothing verifies it. `lib/health.ts`
 * already established the opposite convention in this app — parse the payload,
 * return null when it does not match — and it exists precisely because a
 * misconfigured `API_URL` can point at some other service that answers 200 with
 * a completely different body. Every parser below is that same check, which is
 * what makes `malformed` a real {@link ApiErrorKind} rather than a category that
 * can never occur.
 *
 * The cost is honest: these must be updated by hand when a handler's JSON
 * changes. That is a deliberate trade — a shape change is a breaking API change
 * and should require a visible edit here rather than silently retyping itself.
 *
 * Every type below mirrors a struct in `apps/api/internal/api`: `sessionResponse`
 * and `organizationBody` in auth.go, `projectBody` in projects.go, `boardBody`
 * in boards.go, `columnBody` in columns.go, `cardBody` in cards.go, `memberBody`
 * in auth.go. Timestamps are RFC 3339 strings, rendered by `crud.go`'s
 * `timestamp`; they are kept as strings rather than parsed into `Date` because
 * the only consumer so far is display and a `Date` in an RSC payload is a
 * serialization concern nobody needs yet.
 */

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function str(source: Record<string, unknown>, key: string): string | null {
  const value = source[key];

  return typeof value === "string" ? value : null;
}

function nullableStr(
  source: Record<string, unknown>,
  key: string,
): string | null | undefined {
  const value = source[key];

  if (value === null) {
    return null;
  }

  return typeof value === "string" ? value : undefined;
}

/**
 * Reads `{ "<key>": <object> }`, the envelope every single-resource response
 * uses (`gin.H{subjectCard: ...}` and friends).
 */
export function unwrap(value: unknown, key: string): unknown {
  return isRecord(value) ? value[key] : undefined;
}

/** Builds a parser for `{ "<key>": [ ... ] }`, the list envelope. */
export function parseList<T>(
  key: string,
  parseItem: (value: unknown) => T | null,
): (value: unknown) => T[] | null {
  return (value) => {
    if (!isRecord(value) || !Array.isArray(value[key])) {
      return null;
    }

    const items: T[] = [];

    for (const raw of value[key]) {
      const item = parseItem(raw);

      if (item === null) {
        return null;
      }

      items.push(item);
    }

    return items;
  };
}

/** Parser for endpoints that answer 204 with no body. */
export function parseEmpty(): null {
  return null;
}

/** One organization the signed-in user belongs to. */
export type Organization = {
  id: string;
  name: string;
  slug: string;
  /** `owner` | `admin` | `member`, or "" when the API omitted it. */
  role: string;
};

export function parseOrganization(value: unknown): Organization | null {
  if (!isRecord(value)) {
    return null;
  }

  const id = str(value, "id");
  const name = str(value, "name");
  const slug = str(value, "slug");

  if (id === null || name === null || slug === null) {
    return null;
  }

  // `role` carries `omitempty` on the Go side, so an absent field is normal and
  // means "not stated here", not "malformed".
  return { id, name, slug, role: str(value, "role") ?? "" };
}

/**
 * `POST /auth/login`, `/auth/refresh` and `/auth/organization`.
 *
 * This type exists in exactly two places at runtime: the Route Handlers under
 * `app/api/auth`, and `lib/session/refresh.ts`. It is never returned to a
 * browser — see the module comment on `lib/session/cookies.ts` for why.
 */
export type SessionTokens = {
  tokenType: string;
  accessToken: string;
  /** Access token lifetime in seconds, as the API reports it. */
  expiresIn: number;
  refreshToken: string;
  userId: string;
  organization: Organization;
};

export function parseSessionTokens(value: unknown): SessionTokens | null {
  if (!isRecord(value)) {
    return null;
  }

  const tokenType = str(value, "token_type");
  const accessToken = str(value, "access_token");
  const refreshToken = str(value, "refresh_token");
  const userId = str(value, "user_id");
  const expiresIn = value.expires_in;
  const organization = parseOrganization(value.organization);

  if (
    tokenType === null ||
    accessToken === null ||
    refreshToken === null ||
    userId === null ||
    typeof expiresIn !== "number" ||
    !Number.isFinite(expiresIn) ||
    organization === null
  ) {
    return null;
  }

  return { tokenType, accessToken, expiresIn, refreshToken, userId, organization };
}

/** `POST /auth/register`. Registration does not start a session. */
export type RegisteredUser = {
  userId: string;
  email: string;
  displayName: string;
  organization: Organization;
};

export function parseRegisteredUser(value: unknown): RegisteredUser | null {
  if (!isRecord(value)) {
    return null;
  }

  const userId = str(value, "user_id");
  const email = str(value, "email");
  const displayName = str(value, "display_name");
  const organization = parseOrganization(value.organization);

  if (userId === null || email === null || displayName === null || organization === null) {
    return null;
  }

  return { userId, email, displayName, organization };
}

/** `GET /me`. */
export type CurrentUser = {
  userId: string;
  role: string;
  sessionId: string;
  organization: Organization;
  organizations: Organization[];
};

export function parseCurrentUser(value: unknown): CurrentUser | null {
  if (!isRecord(value)) {
    return null;
  }

  const userId = str(value, "user_id");
  const role = str(value, "role");
  const sessionId = str(value, "session_id");
  const organization = parseOrganization(value.organization);
  const organizations = parseList("organizations", parseOrganization)(value);

  if (
    userId === null ||
    role === null ||
    sessionId === null ||
    organization === null ||
    organizations === null
  ) {
    return null;
  }

  return { userId, role, sessionId, organization, organizations };
}

/** `GET /members`. */
export type Member = {
  membershipId: string;
  userId: string;
  email: string;
  displayName: string;
  role: string;
  joinedAt: string;
};

export function parseMember(value: unknown): Member | null {
  if (!isRecord(value)) {
    return null;
  }

  const membershipId = str(value, "membership_id");
  const userId = str(value, "user_id");
  const email = str(value, "email");
  const displayName = str(value, "display_name");
  const role = str(value, "role");
  const joinedAt = str(value, "joined_at");

  if (
    membershipId === null ||
    userId === null ||
    email === null ||
    displayName === null ||
    role === null ||
    joinedAt === null
  ) {
    return null;
  }

  return { membershipId, userId, email, displayName, role, joinedAt };
}

/**
 * `POST /members` — the membership that was just created.
 *
 * Deliberately narrower than {@link Member}, and the difference is a security
 * property rather than an oversight. `addedMemberBody` in `apps/api` omits the
 * display name because the 201 must carry nothing that was read out of the
 * global directory: the address is the one the caller typed, normalised, and the
 * rest is the row they just created. ADR 0008 spells out why. Keeping the two
 * types separate here means a screen cannot accidentally render a field the API
 * never sent and get `undefined`.
 *
 * The full row, display name included, arrives on the next `GET /members` — by
 * which point it is scoped to the organization the account is now in.
 */
export type AddedMember = {
  membershipId: string;
  userId: string;
  email: string;
  role: string;
  joinedAt: string;
};

export function parseAddedMember(value: unknown): AddedMember | null {
  if (!isRecord(value)) {
    return null;
  }

  const membershipId = str(value, "membership_id");
  const userId = str(value, "user_id");
  const email = str(value, "email");
  const role = str(value, "role");
  const joinedAt = str(value, "joined_at");

  if (
    membershipId === null ||
    userId === null ||
    email === null ||
    role === null ||
    joinedAt === null
  ) {
    return null;
  }

  return { membershipId, userId, email, role, joinedAt };
}

/** A project. `archivedAt` is null while the project is active. */
export type Project = {
  id: string;
  name: string;
  description: string;
  archivedAt: string | null;
  createdAt: string;
  updatedAt: string;
};

export function parseProject(value: unknown): Project | null {
  if (!isRecord(value)) {
    return null;
  }

  const id = str(value, "id");
  const name = str(value, "name");
  const description = str(value, "description");
  const createdAt = str(value, "created_at");
  const updatedAt = str(value, "updated_at");
  const archivedAt = nullableStr(value, "archived_at");

  if (
    id === null ||
    name === null ||
    description === null ||
    createdAt === null ||
    updatedAt === null ||
    archivedAt === undefined
  ) {
    return null;
  }

  return { id, name, description, archivedAt, createdAt, updatedAt };
}

/** A board within a project. */
export type Board = {
  id: string;
  projectId: string;
  name: string;
  createdAt: string;
  updatedAt: string;
};

export function parseBoard(value: unknown): Board | null {
  if (!isRecord(value)) {
    return null;
  }

  const id = str(value, "id");
  const projectId = str(value, "project_id");
  const name = str(value, "name");
  const createdAt = str(value, "created_at");
  const updatedAt = str(value, "updated_at");

  if (id === null || projectId === null || name === null || createdAt === null || updatedAt === null) {
    return null;
  }

  return { id, projectId, name, createdAt, updatedAt };
}

/**
 * A column on a board.
 *
 * There is no position field: `apps/api` returns columns already ordered (see
 * ADR 0004 on card ordering, which columns follow), and exposing a position
 * would invite a client to sort by it and disagree with the server.
 */
export type Column = {
  id: string;
  boardId: string;
  name: string;
  createdAt: string;
  updatedAt: string;
};

export function parseColumn(value: unknown): Column | null {
  if (!isRecord(value)) {
    return null;
  }

  const id = str(value, "id");
  const boardId = str(value, "board_id");
  const name = str(value, "name");
  const createdAt = str(value, "created_at");
  const updatedAt = str(value, "updated_at");

  if (id === null || boardId === null || name === null || createdAt === null || updatedAt === null) {
    return null;
  }

  return { id, boardId, name, createdAt, updatedAt };
}

/** A card in a column. Ordering is the API's, for the same reason as columns. */
export type Card = {
  id: string;
  boardId: string;
  columnId: string;
  title: string;
  description: string;
  createdAt: string;
  updatedAt: string;
};

export function parseCard(value: unknown): Card | null {
  if (!isRecord(value)) {
    return null;
  }

  const id = str(value, "id");
  const boardId = str(value, "board_id");
  const columnId = str(value, "column_id");
  const title = str(value, "title");
  const description = str(value, "description");
  const createdAt = str(value, "created_at");
  const updatedAt = str(value, "updated_at");

  if (
    id === null ||
    boardId === null ||
    columnId === null ||
    title === null ||
    description === null ||
    createdAt === null ||
    updatedAt === null
  ) {
    return null;
  }

  return { id, boardId, columnId, title, description, createdAt, updatedAt };
}
