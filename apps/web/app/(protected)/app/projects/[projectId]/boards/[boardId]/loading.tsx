import { BoardSkeleton } from "@/components/boards/board-skeleton";
import board from "@/components/boards/board.module.css";
import styles from "@/components/workspace/workspace.module.css";

/**
 * What a board page shows while its four requests are in flight.
 *
 * This is the whole loading state, and there is no other. Every fetch on this
 * screen happens on the server before the page renders, so there is nothing to
 * cover with a client-side spinner — Next serves this as the segment's Suspense
 * fallback in the first flush, and it is replaced when the board arrives.
 */
export default function Loading() {
  return (
    <div className={board.page}>
      <div className={styles.header}>
        <div aria-hidden="true" className={styles.skeletonTitle} />
      </div>

      <BoardSkeleton />
    </div>
  );
}
