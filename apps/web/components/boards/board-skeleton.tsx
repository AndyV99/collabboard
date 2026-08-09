import workspace from "@/components/workspace/workspace.module.css";
import styles from "./board.module.css";

/**
 * What the board segment shows while its four requests are in flight.
 *
 * Column-shaped rather than a spinner, so the layout does not jump when the
 * real board arrives — and column-shaped rather than list-shaped, which is what
 * the generic `ListSkeleton` would have given: this segment resolves into a
 * horizontal row of columns, and a placeholder that resolves into a different
 * shape is a worse lie than no placeholder.
 *
 * One `role="status"` line is the only thing announced. The boxes are
 * `aria-hidden`, because a screen reader user should hear "Loading this
 * board…" once rather than being read four empty regions.
 */
export function BoardSkeleton({ columns = 4 }: { columns?: number }) {
  return (
    <div className={workspace.section}>
      <p className={workspace.sectionNote} role="status">
        Loading this board…
      </p>

      <ul aria-hidden="true" className={styles.columns}>
        {Array.from({ length: columns }, (_, index) => (
          <li className={styles.skeletonColumn} key={index} />
        ))}
      </ul>
    </div>
  );
}
