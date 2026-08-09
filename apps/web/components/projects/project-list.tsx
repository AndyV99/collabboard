import Link from "next/link";

import type { Project } from "@/lib/api/types";
import { formatDate } from "@/lib/workspace/format";
import { projectHref } from "@/lib/workspace/routes";
import styles from "@/components/workspace/workspace.module.css";

/**
 * The tenant's active projects.
 *
 * Pure and synchronous: the page fetches, this renders. Every card is a `<Link>`
 * to a real URL rather than a click handler that sets state, which is the whole
 * reason a project survives a reload and can be pasted into a message.
 *
 * `GET /projects` never returns an archived project — `listProjectsHandler` says
 * so, and there is no endpoint that would return one (#49) — so there is no
 * "archived" branch here to be tested and no filter to get wrong. If a project
 * is in this list, it is active.
 */
export function ProjectList({ projects }: { projects: readonly Project[] }) {
  return (
    <ul className={`${styles.list} ${styles.listGrid}`}>
      {projects.map((project) => {
        const created = formatDate(project.createdAt);

        return (
          <li key={project.id}>
            <Link className={styles.card} href={projectHref(project.id)}>
              <span className={styles.cardTitle}>{project.name}</span>

              {project.description !== "" && (
                <span className={styles.cardBody}>{project.description}</span>
              )}

              {created !== null && (
                <span className={styles.cardMeta}>Created {created}</span>
              )}
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
