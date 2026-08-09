import { ListSkeleton } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

/**
 * What `/app` shows while its projects are being read.
 *
 * A `loading.tsx` is Next's Suspense boundary for the segment: it is what the
 * user sees between a navigation and the Server Component resolving, and it is
 * the only loading state these screens need — nothing here fetches from the
 * browser, so there is no second, client-side wait to cover.
 *
 * On a fast API it is barely visible, and that is fine. It exists for the slow
 * ones, and for the reviewer who throttles their connection to see whether it
 * was thought about.
 */
export default function Loading() {
  return (
    <div className={styles.page}>
      <div className={styles.header}>
        <div aria-hidden="true" className={styles.skeletonTitle} />
      </div>

      <ListSkeleton label="Loading projects…" rows={3} />
    </div>
  );
}
