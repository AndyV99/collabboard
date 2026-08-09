import type { Metadata } from "next";

import { getBoard, getProject, listCardsByBoard, listColumns } from "@/lib/api/endpoints";
import type { ApiError } from "@/lib/api/errors";
import { serverApi } from "@/lib/api/server";
import { countCards, groupCardsIntoColumns } from "@/lib/board/snapshot";
import { requireSession } from "@/lib/session/require";
import { describeLoadFailure } from "@/lib/workspace/outcomes";
import { WORKSPACE_PATH, projectHref, selectedCardId } from "@/lib/workspace/routes";
import { BoardView } from "@/components/boards/board-view";
import { PageHeader } from "@/components/workspace/page-header";
import { LoadError } from "@/components/workspace/states";
import board from "@/components/boards/board.module.css";
import styles from "@/components/workspace/workspace.module.css";

type Props = {
  params: Promise<{ projectId: string; boardId: string }>;
  searchParams: Promise<Record<string, string | string[] | undefined>>;
};

export const metadata: Metadata = {
  title: "Board · CollabBoard",
};

/**
 * A board: its columns, every card on it, and whichever card the URL has open.
 *
 * # Four requests, all at once, and never one per card
 *
 * `GET /boards/:id`, `GET /projects/:id`, `GET /boards/:id/columns` and
 * `GET /boards/:id/cards`. That is the whole page — a board with four columns
 * and two hundred cards makes exactly the same four requests as an empty one,
 * because the cards endpoint answers with the entire board (`ListCardsByBoard`,
 * served by `cards_tenant_board_idx`) rather than per column. Fetching per
 * column would be O(columns) and fetching per card O(cards); both are wrong,
 * and the second is wrong by two orders of magnitude here.
 *
 * They go out together because none of them depends on another's answer. The
 * tempting version — read the board, then read its columns and cards only if it
 * exists — is a waterfall that doubles time to first paint on every successful
 * load in order to save two requests on the rare failing one. Those two are
 * harmless when the board is not the caller's: `listColumnsByBoardHandler` and
 * `listCardsByBoardHandler` are ordinary tenant-scoped reads, so another
 * tenant's board id returns an empty list rather than anything to leak.
 *
 * The project is read for the breadcrumb, exactly as #62 established, and for
 * the check below.
 *
 * # 404 is the board's answer, not the cards'
 *
 * Only `GET /boards/:id` distinguishes a board that does not exist from one
 * that does. The two list endpoints answer 200 with `[]` for an unknown board
 * id, because they filter by `board_id` inside the tenant's row-level security
 * rather than resolving the board first. So a missing board and another
 * tenant's board are both the board request's 404 — one state, one sentence,
 * for the reason `lib/workspace/outcomes.ts` explains — and an empty cards list
 * genuinely means an empty board.
 *
 * # The project id in the URL is checked, not trusted
 *
 * `apps/api` addresses a board flatly at `/boards/:id` and takes no project
 * argument, so `/app/projects/<any project>/boards/<this board>` would
 * otherwise render a real board underneath a breadcrumb naming a project it is
 * not in. `board.projectId` must equal the segment it was reached through; a
 * mismatch renders not-found rather than a plausible-looking lie. This is not a
 * security control — both requests were already scoped to the caller's tenant —
 * it is the difference between a URL that means what it says and one that only
 * usually does.
 *
 * # Where the Server/Client line falls, for #65 and #66
 *
 * Here, and only here. This page is the sole thing on the board screen that
 * touches the API or the session; `BoardView` and everything under it take
 * resolved, plain-serialisable props and fetch nothing.
 *
 * #64 made the board editable and this file did not have to change shape to
 * allow it — which was the prediction #63 wrote down. `BoardView` gained
 * `"use client"`, the props crossed the RSC boundary unchanged because `Card`
 * and `Column` are strings all the way down, and the reads stayed here. The two
 * real edits were handing down the raw `?card=` value instead of a resolved one
 * (a card can now be deleted, so "names nothing on this board" has to be
 * decided where the optimistic board is known) and letting `BoardView` own the
 * detail panel so that one optimistic store covers the tile and the panel that
 * edits it.
 *
 * Writes do **not** come back through here. A Client Component reaches the API
 * through `/api/proxy`, which attaches the token server-side, and then calls
 * `router.refresh()` — so this function re-runs and the board re-renders from
 * what the server actually stored. `components/boards/board-view.tsx` states
 * the rest of the rules that come with that boundary.
 */
export default async function BoardPage({ params, searchParams }: Props) {
  await requireSession();

  const [{ projectId, boardId }, query] = await Promise.all([params, searchParams]);

  const [boardResult, projectResult, columnsResult, cardsResult] = await Promise.all([
    serverApi(getBoard(boardId)),
    serverApi(getProject(projectId)),
    serverApi(listColumns(boardId)),
    serverApi(listCardsByBoard(boardId)),
  ]);

  const notFound = (
    <div className={styles.page}>
      <PageHeader
        crumbs={[{ href: WORKSPACE_PATH, label: "Projects" }, { label: "Not found" }]}
        title="Board"
      />
      <LoadError
        failure={{
          title: "Not found",
          message:
            "This board does not exist, it is not in this project, or it belongs to a workspace you are not a member of.",
          retryable: false,
        }}
      />
    </div>
  );

  if (!boardResult.ok) {
    // A 404 gets the sentence above, which covers the cross-tenant case the API
    // deliberately makes indistinguishable. Anything else — an outage, a
    // rate limit — is a different message and, where it makes sense, a retry.
    if (boardResult.error.kind === "not_found") {
      return notFound;
    }

    return (
      <div className={styles.page}>
        <PageHeader
          crumbs={[{ href: WORKSPACE_PATH, label: "Projects" }, { label: "Board" }]}
          title="Board"
        />
        <LoadError failure={describeLoadFailure(boardResult.error, "board")} />
      </div>
    );
  }

  if (!projectResult.ok || boardResult.data.projectId !== projectId) {
    return notFound;
  }

  const theBoard = boardResult.data;
  const project = projectResult.data;

  const crumbs = [
    { href: WORKSPACE_PATH, label: "Projects" },
    { href: projectHref(project.id), label: project.name },
    { label: theBoard.name },
  ];

  // The board loaded and its contents did not. This is a real, separable state:
  // the header and breadcrumb are correct and worth keeping on screen, and the
  // failure is about the columns and cards rather than about the board.
  const contentsFailed = (error: ApiError) => (
    <div className={styles.page}>
      <PageHeader crumbs={crumbs} title={theBoard.name} />
      <LoadError failure={describeLoadFailure(error, "columns and cards")} />
    </div>
  );

  // Reported one at a time rather than both: if the two failed together they
  // almost certainly failed for the same reason, and two stacked panels saying
  // the same sentence is not twice the help.
  if (!columnsResult.ok) {
    return contentsFailed(columnsResult.error);
  }

  if (!cardsResult.ok) {
    return contentsFailed(cardsResult.error);
  }

  const columns = columnsResult.data;
  const cards = cardsResult.data;
  const snapshot = groupCardsIntoColumns(columns, cards);
  const total = countCards(snapshot);

  const openCardId = selectedCardId(query);

  return (
    <div className={board.page}>
      <PageHeader
        crumbs={crumbs}
        lede={`${describeCount(columns.length, "column")}, ${describeCount(total, "card")}.`}
        title={theBoard.name}
      />

      {/* The whole interactive surface, including the card detail panel, is one
       * client boundary. It has to be one rather than two: the panel edits the
       * same card the board draws a tile for, and two optimistic stores would
       * show a renamed card in one of them and the old name in the other until
       * the refresh arrived. See the comment at the top of `board-view.tsx`. */}
      <BoardView
        boardId={theBoard.id}
        projectId={project.id}
        selectedCardId={openCardId}
        snapshot={snapshot}
      />

      {snapshot.unplaced.length > 0 && (
        <section className={styles.panel}>
          <h2 className={styles.panelTitle}>
            {describeCount(snapshot.unplaced.length, "card")} not shown
          </h2>
          <p className={styles.panelBody}>
            The columns and the cards are two separate requests, so a column created
            between them is missing from one and has cards in the other. Reload to pick
            it up. These cards are counted above but have nowhere to be drawn — they are
            reported rather than dropped, because a board that quietly loses a card is
            worse than one that says it has.
          </p>
        </section>
      )}

      <p className={styles.sectionNote}>
        Cards appear in the order the server returns them — the rank behind that order
        is never sent to the browser, by design (ADR 0004), so a column is reordered by
        naming its new neighbour and re-reading rather than by sorting here. Cards
        cannot be dragged between columns yet: that is issue #65. Live updates from
        other people editing this board are #9, so a colleague&rsquo;s change appears
        on your next reload rather than as it happens.
      </p>
    </div>
  );
}

/** "1 column" / "4 columns", so the lede does not read like a spreadsheet. */
function describeCount(count: number, noun: string): string {
  return `${count} ${noun}${count === 1 ? "" : "s"}`;
}
