import Link from "next/link";

import type { Board } from "@/lib/api/types";
import { formatDate } from "@/lib/workspace/format";
import { boardHref } from "@/lib/workspace/routes";
import styles from "@/components/workspace/workspace.module.css";

/**
 * A project's boards.
 *
 * The link carries the project id as well as the board id — see
 * `lib/workspace/routes.ts` for why the URL is nested even though the API
 * addresses a board flatly.
 */
export function BoardList({
  boards,
  projectId,
}: {
  boards: readonly Board[];
  projectId: string;
}) {
  return (
    <ul className={`${styles.list} ${styles.listGrid}`}>
      {boards.map((board) => {
        const created = formatDate(board.createdAt);

        return (
          <li key={board.id}>
            <Link className={styles.card} href={boardHref(projectId, board.id)}>
              <span className={styles.cardTitle}>{board.name}</span>

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
