"use client";

import { useRouter } from "next/navigation";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import { api } from "@/lib/api/browser";
import { archiveProject } from "@/lib/api/endpoints";
import type { Project } from "@/lib/api/types";
import { describeWriteFailure } from "@/lib/workspace/outcomes";
import { WORKSPACE_PATH } from "@/lib/workspace/routes";
import { FormMessage, TextField } from "@/components/workspace/fields";
import styles from "@/components/workspace/workspace.module.css";

/**
 * Archiving a project, which in this version is permanent.
 *
 * # The decision this component encodes
 *
 * Issue #62 offers two ways to handle a one-way door: hide the control until
 * #49 lands, or make the consequence explicit. This takes the second, and the
 * reason is that hiding it does not make the door reversible — it makes the
 * capability unreachable from the UI while leaving `POST /projects/:id/archive`
 * exactly as final as it was. A user who never sees the control cannot be
 * stranded by it, but they also cannot tidy a workspace, and the day #49 lands
 * the UI has to be designed anyway.
 *
 * So the consequence is stated in full, before the fact, in the place where it
 * can still change the decision:
 *
 * - **It is not undoable.** There is no unarchive endpoint and no way to list
 *   archived projects, so the project is gone from every list in the product.
 * - **Nothing is deleted.** `ArchiveProject` sets `archived_at`; the boards and
 *   cards are still in the database. This matters because "archive" reads like
 *   "delete" to a lot of people, and being honest in the other direction is what
 *   makes the warning credible.
 * - **This page's URL keeps working.** `GetProject` does not filter on
 *   `archived_at`, so a saved link still opens the project, marked archived. It
 *   is the only route back to those boards, so it is worth saying *before* the
 *   confirmation rather than discovering afterwards.
 *
 * # Why it asks you to type the name
 *
 * Because a confirm dialog is a reflex and typing is a decision. It is the
 * pattern every irreversible destructive action in this industry converged on
 * for the same reason, and the cost — a few seconds, once — is trivial against
 * an action with no undo. It also means an accidental Enter key cannot do it:
 * the submit is disabled until the field matches.
 *
 * The whole thing sits inside a collapsed `<details>`, so the control is one
 * deliberate expansion away rather than sitting under a cursor on the page a
 * user opened to rename something.
 */
export function ArchiveProject({ project }: { project: Project }) {
  const router = useRouter();
  const confirmId = `${useId()}-confirm`;

  const [typed, setTyped] = useState("");
  const [failure, setFailure] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [attempt, setAttempt] = useState(0);

  const messageRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (attempt > 0) {
      messageRef.current?.focus();
    }
  }, [attempt]);

  // Trimmed, because a name pasted from elsewhere often arrives with a space on
  // the end and refusing that would look like the check is broken rather than
  // strict. The comparison itself is exact and case-sensitive.
  const confirmed = typed.trim() === project.name;

  async function handleSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();

    if (pending || !confirmed) {
      return;
    }

    setPending(true);
    setFailure(null);

    const result = await api(archiveProject(project.id));

    if (!result.ok) {
      setPending(false);
      setFailure(describeWriteFailure(result.error, "archive this project"));
      setAttempt((count) => count + 1);

      return;
    }

    // Left pending through the navigation. The project list is where the
    // consequence is visible — the project is not in it — so that is where the
    // user is sent, rather than being left on a page that now describes
    // something they can no longer find.
    router.push(WORKSPACE_PATH);
    router.refresh();
  }

  return (
    <details className={`${styles.disclosure} ${styles.disclosureDanger}`}>
      <summary className={styles.disclosureSummary}>Archive this project</summary>

      <div className={styles.disclosureBody}>
        <form className={styles.form} noValidate onSubmit={handleSubmit}>
          {failure !== null && (
            <FormMessage messageRef={messageRef} title="Could not archive the project">
              <p>{failure}</p>
            </FormMessage>
          )}

          <div className={styles.panelBody}>
            <p>
              <strong>Archiving cannot be undone.</strong> This version of
              CollabBoard has no way to restore an archived project and no way to
              list the archived ones, so <strong>{project.name}</strong> will
              disappear from the project list for everyone in this workspace and
              will not appear anywhere else.
            </p>

            <p>
              Its boards and cards are <strong>not deleted</strong> — they stay in
              the database, and this page&rsquo;s address keeps working. Save the
              link first if you might want to reach them again; it is the only
              route back.
            </p>
          </div>

          <TextField
            disabled={pending}
            hint={
              <>
                Type <strong>{project.name}</strong> to confirm. This is the only
                thing standing between a stray click and a project nobody can
                find.
              </>
            }
            id={confirmId}
            label="Project name"
            onChange={setTyped}
            value={typed}
          />

          <button
            className={styles.danger}
            disabled={pending || !confirmed}
            type="submit"
          >
            {pending ? "Archiving…" : "Archive permanently"}
          </button>
        </form>
      </div>
    </details>
  );
}
