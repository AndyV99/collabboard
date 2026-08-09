import type { Metadata } from "next";
import Link from "next/link";

import { getProject, listBoards } from "@/lib/api/endpoints";
import { serverApi } from "@/lib/api/server";
import { requireSession } from "@/lib/session/require";
import { formatDate } from "@/lib/workspace/format";
import { describeLoadFailure } from "@/lib/workspace/outcomes";
import { WORKSPACE_PATH } from "@/lib/workspace/routes";
import { BoardList } from "@/components/boards/board-list";
import { CreateBoardForm } from "@/components/boards/create-board-form";
import { ArchiveProject } from "@/components/projects/archive-project";
import { RenameProjectForm } from "@/components/projects/rename-project-form";
import { PageHeader } from "@/components/workspace/page-header";
import { EmptyState, LoadError } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

type Params = { params: Promise<{ projectId: string }> };

export const metadata: Metadata = {
  title: "Project · CollabBoard",
};

/**
 * One project: its boards, its details, and the way out of it.
 *
 * # Two requests, in parallel, and no waterfall
 *
 * The project and its boards are independent — `listBoardsHandler` takes the id
 * from the path and never reads the project — so they go out together. Awaiting
 * one before the other would double the time to first paint for no reason.
 *
 * # A 404 here is deliberately ambiguous, and the copy keeps it that way
 *
 * `apps/api` answers 404 for an id that never existed *and* for one belonging to
 * another organization, so that this endpoint is not an existence oracle across
 * the tenant boundary. This page cannot distinguish them and does not pretend
 * to: one state, one sentence, covering both. That is also the state a deep link
 * from a colleague in another workspace lands on.
 *
 * # Archived projects are still reachable by URL
 *
 * `GetProject` has no `archived_at` predicate — only `ListProjects` does — so an
 * archived project keeps answering on its own address even though it is in no
 * list. That is worth surfacing rather than smoothing over: it is the only route
 * back to the boards inside it, and someone who arrives here after archiving
 * needs to be told which of the two states they are in. The write controls come
 * off, because renaming something nobody can find is not a useful offer.
 */
export default async function ProjectPage({ params }: Params) {
  await requireSession();

  const { projectId } = await params;

  const [projectResult, boardsResult] = await Promise.all([
    serverApi(getProject(projectId)),
    serverApi(listBoards(projectId)),
  ]);

  if (!projectResult.ok) {
    return (
      <div className={styles.page}>
        <PageHeader
          crumbs={[{ href: WORKSPACE_PATH, label: "Projects" }, { label: "Not found" }]}
          title="Project"
        />
        <LoadError failure={describeLoadFailure(projectResult.error, "project")} />
      </div>
    );
  }

  const project = projectResult.data;
  const archived = project.archivedAt !== null;
  const archivedOn = formatDate(project.archivedAt);

  return (
    <div className={styles.page}>
      <PageHeader
        crumbs={[{ href: WORKSPACE_PATH, label: "Projects" }, { label: project.name }]}
        lede={project.description === "" ? undefined : project.description}
        title={project.name}
      />

      {archived && (
        <section className={`${styles.panel} ${styles.panelDanger}`}>
          <h2 className={styles.panelTitle}>This project is archived</h2>
          <div className={styles.panelBody}>
            <p>
              It was archived{archivedOn === null ? "" : ` on ${archivedOn}`}, so it no
              longer appears in the project list and there is no way to restore it or to
              browse the archived ones. You reached it through its address, which keeps
              working — <strong>save this link</strong> if you need the boards below
              again.
            </p>
            <p>
              Nothing was deleted. Renaming and archiving are switched off here because
              neither would make the project findable again.
            </p>
          </div>
        </section>
      )}

      <section className={styles.section}>
        <h2 className={styles.sectionHeading}>Boards</h2>

        {!boardsResult.ok ? (
          <LoadError failure={describeLoadFailure(boardsResult.error, "boards")} />
        ) : boardsResult.data.length === 0 ? (
          <EmptyState
            body={
              <p>
                A board is one workflow — columns you move cards between. Most projects
                have a handful: one per sprint, per launch, or per pipeline.
              </p>
            }
            title={archived ? "This project has no boards" : "No boards in this project yet"}
          >
            {!archived && <CreateBoardForm autoFocus projectId={project.id} />}
          </EmptyState>
        ) : (
          <>
            <BoardList boards={boardsResult.data} projectId={project.id} />

            {!archived && (
              <details className={styles.disclosure}>
                <summary className={styles.disclosureSummary}>New board</summary>
                <div className={styles.disclosureBody}>
                  <CreateBoardForm projectId={project.id} />
                </div>
              </details>
            )}
          </>
        )}
      </section>

      {!archived && (
        <section className={styles.section}>
          <h2 className={styles.sectionHeading}>Project details</h2>

          <div className={styles.panel}>
            <RenameProjectForm project={project} />
          </div>

          <ArchiveProject project={project} />
        </section>
      )}

      <p className={styles.sectionNote}>
        <Link href={WORKSPACE_PATH}>Back to all projects</Link>
      </p>
    </div>
  );
}
