import { ListSkeleton } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

/** What a project page shows while the project and its boards are read. */
export default function Loading() {
  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div aria-hidden="true" className={styles.skeletonTitle} />
      </div>

      <ListSkeleton label="Loading this project…" rows={2} />
    </div>
  );
}
