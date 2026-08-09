import { ListSkeleton } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

/** What the people page shows while the member list is read. */
export default function Loading() {
  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div aria-hidden="true" className={styles.skeletonTitle} />
      </div>

      <ListSkeleton label="Loading the people in this workspace…" rows={3} />
    </div>
  );
}
