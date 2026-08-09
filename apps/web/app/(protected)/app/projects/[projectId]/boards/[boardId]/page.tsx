import type { Metadata } from "next";

import { getBoard, getProject } from "@/lib/api/endpoints";
import { serverApi } from "@/lib/api/server";
import { requireSession } from "@/lib/session/require";
import { formatDate } from "@/lib/workspace/format";
import { describeLoadFailure } from "@/lib/workspace/outcomes";
import { WORKSPACE_PATH, projectHref } from "@/lib/workspace/routes";
import { PageHeader } from "@/components/workspace/page-header";
import { LoadError } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

type Params = { params: Promise<{ projectId: string; boardId: string }> };

export const metadata: Metadata = {
  title: "Board · CollabBoard",
};

/**
 * One board — deliberately without the board.
 *
 * Columns, cards and drag-and-drop are issue #63, and building a stand-in for
 * them here would be work thrown away plus a second design to reconcile. What
 * this page does prove is the part #62 is responsible for: that a board has an
 * address, that the address survives a reload, that it can be pasted to a
 * colleague, and that the breadcrumb above it is not a guess.
 *
 * # The project id in the URL is checked, not trusted
 *
 * `apps/api` addresses a board flatly at `/boards/:id` and takes no project
 * argument, so `/app/projects/<any project>/boards/<this board>` would otherwise
 * render a real board underneath a breadcrumb naming a project it is not in.
 * Both ids are resolved and `board.projectId` must equal the segment they were
 * reached through; a mismatch renders the not-found state rather than a
 * plausible-looking lie.
 *
 * That check costs nothing in security terms — both requests are already scoped
 * to the caller's tenant, and a board in another organization is a 404 from the
 * API before this code runs — but it is the difference between a URL that means
 * what it says and one that only usually does.
 */
export default async function BoardPage({ params }: Params) {
  await requireSession();

  const { projectId, boardId } = await params;

  const [boardResult, projectResult] = await Promise.all([
    serverApi(getBoard(boardId)),
    serverApi(getProject(projectId)),
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

  const board = boardResult.data;
  const project = projectResult.data;
  const created = formatDate(board.createdAt);

  return (
    <div className={styles.page}>
      <PageHeader
        crumbs={[
          { href: WORKSPACE_PATH, label: "Projects" },
          { href: projectHref(project.id), label: project.name },
          { label: board.name },
        ]}
        lede={created === null ? undefined : `Created ${created}.`}
        title={board.name}
      />

      <section className={styles.panel}>
        <h2 className={styles.panelTitle}>The board itself is not built yet</h2>

        <div className={styles.panelBody}>
          <p>
            Columns, cards and drag-and-drop are <strong>issue #63</strong>, and live
            updates over the WebSocket are <strong>issue #9</strong>. This page exists
            so that the route around them is real: the address you are on identifies
            this board inside this project, it survives a reload, and it opens the same
            screen for anyone in this workspace who follows it.
          </p>
        </div>
      </section>
    </div>
  );
}
