import { ListSkeleton } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

/** What a board page shows while the board and its project are read. */
export default function Loading() {
  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div aria-hidden="true" className={styles.skeletonTitle} />
      </div>

      <ListSkeleton label="Loading this board…" rows={1} />
    </div>
  );
}
