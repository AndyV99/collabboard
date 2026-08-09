/**
 * The typed surface of the API, as data.
 *
 * Every function here returns an {@link Endpoint} — a description of a request
 * and how to parse its answer — rather than performing one. That is what lets
 * the same definitions serve three callers that reach the API very differently:
 *
 *   - `lib/api/server.ts` sends them to the Go API with a bearer token;
 *   - `lib/api/browser.ts` sends them to this app's own `/api/proxy`, which
 *     attaches the token server-side so the browser never holds one;
 *   - the tests send them to a stub with no network at all.
 *
 * Paths are relative to `/api/v1` (see {@link API_V1_PREFIX}).
 *
 * # What is deliberately not here
 *
 * `POST /auth/login`, `/auth/register`, `/auth/refresh`, `/auth/logout` and
 * `/auth/organization`. Those five return or consume a refresh token, and a
 * refresh token must never be reachable from a browser. They are reached only
 * through `lib/session`, which runs on the server, and the browser talks to
 * them through the Route Handlers in `app/api/auth` that keep the tokens in
 * httpOnly cookies. `app/api/proxy` refuses the `auth` prefix outright, so
 * there is no path from client JavaScript to any of them.
 */

import type { Endpoint } from "./http";
import {
  type AddedMember,
  type Board,
  type Card,
  type Column,
  type CurrentUser,
  type Member,
  type Project,
  parseAddedMember,
  parseBoard,
  parseCard,
  parseColumn,
  parseCurrentUser,
  parseEmpty,
  parseList,
  parseMember,
  parseProject,
  unwrap,
} from "./types";

/** Builds a parser for the `{ "<key>": <object> }` single-resource envelope. */
function envelope<T>(key: string, parse: (value: unknown) => T | null) {
  return (value: unknown) => parse(unwrap(value, key));
}

const project = envelope("project", parseProject);
const board = envelope("board", parseBoard);
const column = envelope("column", parseColumn);
const card = envelope("card", parseCard);

/** Path segment encoder. Ids are uuids, but nothing here assumes that. */
function seg(value: string): string {
  return encodeURIComponent(value);
}

/** `GET /me` — the signed-in principal and the organizations it can act in. */
export function currentUser(): Endpoint<CurrentUser> {
  return { method: "GET", path: "/me", parse: parseCurrentUser };
}

/** `GET /members` — everyone in the token's organization. */
export function listMembers(): Endpoint<Member[]> {
  return { method: "GET", path: "/members", parse: parseList("members", parseMember) };
}

/**
 * `POST /members` — put an existing account into the caller's organization.
 *
 * There is no organization field and deliberately nowhere to put one: the
 * tenant is the caller's verified org claim, on this side of the wire as on the
 * other. See ADR 0008 for why this is a direct add rather than an invitation,
 * and `apps/api/internal/auth/members.go` for the statuses — 403 when the
 * caller's role does not permit it, 404 when the address has no account, 409
 * when the account is already a member.
 *
 * `role` is omitted rather than sent empty when the caller does not choose one;
 * `JSON.stringify` drops an `undefined` value, and the API reads an absent role
 * as `member`.
 */
export function addMember(input: {
  email: string;
  role?: string;
}): Endpoint<AddedMember> {
  return {
    method: "POST",
    path: "/members",
    body: input,
    parse: envelope("member", parseAddedMember),
  };
}

export function listProjects(): Endpoint<Project[]> {
  return {
    method: "GET",
    path: "/projects",
    parse: parseList("projects", parseProject),
  };
}

export function getProject(projectId: string): Endpoint<Project> {
  return { method: "GET", path: `/projects/${seg(projectId)}`, parse: project };
}

export function createProject(input: {
  name: string;
  description?: string;
}): Endpoint<Project> {
  return { method: "POST", path: "/projects", body: input, parse: project };
}

/**
 * `PATCH /projects/:id`.
 *
 * Fields are optional and an omitted field means "leave it alone" — the Go
 * handler takes `*string` and passes nil through to the query. Sending
 * `undefined` therefore has to *omit* the key, which `JSON.stringify` does for
 * us; sending null would be a different request.
 */
export function updateProject(
  projectId: string,
  input: { name?: string; description?: string },
): Endpoint<Project> {
  return {
    method: "PATCH",
    path: `/projects/${seg(projectId)}`,
    body: input,
    parse: project,
  };
}

/** `POST /projects/:id/archive`. Archiving is not a DELETE: the row stays. */
export function archiveProject(projectId: string): Endpoint<Project> {
  return {
    method: "POST",
    path: `/projects/${seg(projectId)}/archive`,
    parse: project,
  };
}

export function listBoards(projectId: string): Endpoint<Board[]> {
  return {
    method: "GET",
    path: `/projects/${seg(projectId)}/boards`,
    parse: parseList("boards", parseBoard),
  };
}

export function getBoard(boardId: string): Endpoint<Board> {
  return { method: "GET", path: `/boards/${seg(boardId)}`, parse: board };
}

export function createBoard(
  projectId: string,
  input: { name: string },
): Endpoint<Board> {
  return {
    method: "POST",
    path: `/projects/${seg(projectId)}/boards`,
    body: input,
    parse: board,
  };
}

export function updateBoard(
  boardId: string,
  input: { name?: string },
): Endpoint<Board> {
  return { method: "PATCH", path: `/boards/${seg(boardId)}`, body: input, parse: board };
}

export function deleteBoard(boardId: string): Endpoint<null> {
  return {
    method: "DELETE",
    path: `/boards/${seg(boardId)}`,
    parse: parseEmpty,
    expectNoContent: true,
  };
}

export function listColumns(boardId: string): Endpoint<Column[]> {
  return {
    method: "GET",
    path: `/boards/${seg(boardId)}/columns`,
    parse: parseList("columns", parseColumn),
  };
}

export function createColumn(
  boardId: string,
  input: { name: string },
): Endpoint<Column> {
  return {
    method: "POST",
    path: `/boards/${seg(boardId)}/columns`,
    body: input,
    parse: column,
  };
}

export function updateColumn(
  columnId: string,
  input: { name?: string },
): Endpoint<Column> {
  return {
    method: "PATCH",
    path: `/columns/${seg(columnId)}`,
    body: input,
    parse: column,
  };
}

/**
 * `POST /columns/:id/move`.
 *
 * `afterColumnId: null` means "put it first" — a position no sibling's id can
 * name, which is why the field is nullable rather than absent (`optionalUUID`
 * in crud.go treats null and "" the same way).
 */
export function moveColumn(
  columnId: string,
  input: { afterColumnId: string | null },
): Endpoint<Column> {
  return {
    method: "POST",
    path: `/columns/${seg(columnId)}/move`,
    body: { after_column_id: input.afterColumnId },
    parse: column,
  };
}

export function deleteColumn(columnId: string): Endpoint<null> {
  return {
    method: "DELETE",
    path: `/columns/${seg(columnId)}`,
    parse: parseEmpty,
    expectNoContent: true,
  };
}

export function listCardsByColumn(columnId: string): Endpoint<Card[]> {
  return {
    method: "GET",
    path: `/columns/${seg(columnId)}/cards`,
    parse: parseList("cards", parseCard),
  };
}

/** `GET /boards/:id/cards` — every card on a board, for the initial render. */
export function listCardsByBoard(boardId: string): Endpoint<Card[]> {
  return {
    method: "GET",
    path: `/boards/${seg(boardId)}/cards`,
    parse: parseList("cards", parseCard),
  };
}

export function getCard(cardId: string): Endpoint<Card> {
  return { method: "GET", path: `/cards/${seg(cardId)}`, parse: card };
}

export function createCard(
  columnId: string,
  input: { title: string; description?: string },
): Endpoint<Card> {
  return {
    method: "POST",
    path: `/columns/${seg(columnId)}/cards`,
    body: input,
    parse: card,
  };
}

export function updateCard(
  cardId: string,
  input: { title?: string; description?: string },
): Endpoint<Card> {
  return { method: "PATCH", path: `/cards/${seg(cardId)}`, body: input, parse: card };
}

/**
 * `POST /cards/:id/move`.
 *
 * A POST rather than a PATCH because the arguments — the target column and the
 * anchor — are not properties of the card. A stale anchor is a 409, which is
 * why `conflict` is one of the error kinds a board screen has to handle.
 */
export function moveCard(
  cardId: string,
  input: { columnId: string; afterCardId: string | null },
): Endpoint<Card> {
  return {
    method: "POST",
    path: `/cards/${seg(cardId)}/move`,
    body: { column_id: input.columnId, after_card_id: input.afterCardId },
    parse: card,
  };
}

export function deleteCard(cardId: string): Endpoint<null> {
  return {
    method: "DELETE",
    path: `/cards/${seg(cardId)}`,
    parse: parseEmpty,
    expectNoContent: true,
  };
}
