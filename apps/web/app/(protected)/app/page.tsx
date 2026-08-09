import type { Metadata } from "next";
import Link from "next/link";

import { listProjects } from "@/lib/api/endpoints";
import { serverApi } from "@/lib/api/server";
import { requireSession } from "@/lib/session/require";
import { describeLoadFailure } from "@/lib/workspace/outcomes";
import { MEMBERS_PATH } from "@/lib/workspace/routes";
import { CreateProjectForm } from "@/components/projects/create-project-form";
import { ProjectList } from "@/components/projects/project-list";
import { PageHeader } from "@/components/workspace/page-header";
import { EmptyState, LoadError, Steps } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

export const metadata: Metadata = {
  title: "Projects · CollabBoard",
};

/**
 * Where signing in lands you: the workspace's projects.
 *
 * A Server Component that reads the session and fetches in one pass — no
 * client-side fetch, no loading state on the happy path, and nothing about the
 * API's location in the browser bundle. `loading.tsx` next to this file covers
 * the wait; `serverApi` returning a failure covers the rest.
 *
 * `requireSession()` runs in the protected layout as well. Calling it again
 * costs a cookie read and no request, and it means this page states its own
 * requirement rather than inheriting one silently.
 *
 * # The empty state is the first screen a new account sees
 *
 * So it is a screen rather than an absence. It says what a project *is* — the
 * vocabulary this app uses is not self-evident, and "project → board → card" is
 * three words that save a lot of clicking — and it puts the create form in front
 * of the user with focus already in it. There is exactly one thing to do here
 * and it is the thing on screen.
 */
export default async function ProjectsPage() {
  const session = await requireSession();
  const result = await serverApi(listProjects());

  if (!result.ok) {
    return (
      <div className={styles.page}>
        <PageHeader title="Projects" />
        <LoadError failure={describeLoadFailure(result.error, "projects")} />
      </div>
    );
  }

  const projects = result.data;

  if (projects.length === 0) {
    return (
      <div className={styles.page}>
        <PageHeader
          lede={
            <>
              Everything you create belongs to <strong>{session.organization.name}</strong>,
              and only its members can see it.
            </>
          }
          title="Projects"
        />

        <EmptyState
          body={
            <>
              <p>
                This workspace does not have any projects yet. A project is the
                container for a piece of work — a product, a client, a quarter —
                and everything else lives inside one.
              </p>

              <Steps
                steps={[
                  {
                    title: "Create a project.",
                    body: "One per product, client or team. You can rename it later.",
                  },
                  {
                    title: "Add a board to it.",
                    body: "One board per workflow — a sprint, a launch, a hiring pipeline.",
                  },
                  {
                    title: "Add the people it is for.",
                    body: "Everyone in this workspace can see every project in it.",
                  },
                ]}
              />
            </>
          }
          title="Start with your first project"
        >
          <CreateProjectForm autoFocus />
        </EmptyState>

        <p className={styles.sectionNote}>
          Working with other people? <Link href={MEMBERS_PATH}>Add them to the workspace</Link>{" "}
          — they will see these projects as soon as they are in.
        </p>
      </div>
    );
  }

  return (
    <div className={styles.page}>
      <PageHeader
        lede={
          <>
            {projects.length === 1 ? "One project" : `${projects.length} projects`} in{" "}
            <strong>{session.organization.name}</strong>. Open one to see its boards.
          </>
        }
        title="Projects"
      />

      <ProjectList projects={projects} />

      <details className={styles.disclosure}>
        <summary className={styles.disclosureSummary}>New project</summary>
        <div className={styles.disclosureBody}>
          <CreateProjectForm />
        </div>
      </details>
    </div>
  );
}
