/**
 * The URLs of the signed-in app.
 *
 * # Every screen is addressable, and that is the point
 *
 * Issue #62 asks for navigation "with URLs that survive a reload and can be
 * shared". That is a statement about where state lives: the project you are
 * looking at and the board you are looking at are in the path, not in a
 * selection held in a client component. So a reload re-renders the same screen
 * from the server, a pasted link opens the same screen for a colleague who has
 * access, and the back button means what it looks like it means.
 *
 * It also means the ids are attacker-supplied — a link is just a string someone
 * can send — which is fine and is why this file does no validation. An id that
 * names nothing, or names another tenant's project, is resolved inside the
 * caller's own tenant by `apps/api` and comes back as a 404 either way (see
 * `crud.go`'s `notFound`). The screens render that as one honest state rather
 * than guessing which of the two it was.
 *
 * # Why the board path is nested under its project
 *
 * `apps/api` addresses a board flatly, at `/boards/:id`, and deliberately so:
 * a nested API path invites a handler to trust the ancestors in it. A *URL* has
 * the opposite problem to solve. It has to render a breadcrumb, and a breadcrumb
 * needs the project. Fetching the board and then its project would be two round
 * trips to draw one line of text.
 *
 * So the project id is in the URL, and the board page checks it: `board.projectId`
 * must equal the segment it was reached through, or the page renders its
 * not-found state. A mismatched pair is a link that would otherwise show a real
 * board underneath a breadcrumb naming a project it is not in.
 */

/** Where signing in lands, and the root of everything below. */
export const WORKSPACE_PATH = "/app";

/** The organization's people. */
export const MEMBERS_PATH = "/app/members";

/**
 * Path segments are encoded on the way in.
 *
 * Ids are uuids in practice, but these values arrive from an API response and
 * from route parameters, and a helper that assumes a shape it does not check is
 * a helper that builds a broken URL the day the assumption stops holding.
 */
function seg(value: string): string {
  return encodeURIComponent(value);
}

/** One project: its boards, its name, and the archive control. */
export function projectHref(projectId: string): string {
  return `${WORKSPACE_PATH}/projects/${seg(projectId)}`;
}

/** One board, reached through the project it belongs to. */
export function boardHref(projectId: string, boardId: string): string {
  return `${projectHref(projectId)}/boards/${seg(boardId)}`;
}
