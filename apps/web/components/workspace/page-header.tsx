import Link from "next/link";
import type { ReactNode } from "react";

import styles from "./workspace.module.css";

/** One step in the trail. The last one is the current page and is not a link. */
export type Crumb = { label: string; href?: string };

/**
 * The trail back up the hierarchy.
 *
 * A `<nav>` with an accessible name, because a page can have several landmarks
 * of the same type and "navigation" on its own tells a screen reader user
 * nothing about which one they have landed in. The current page is marked with
 * `aria-current="page"` rather than merely being unlinked.
 *
 * The separators are `aria-hidden`: they are punctuation, and a screen reader
 * announcing "slash" between every step is noise the list structure already
 * conveys.
 */
export function Breadcrumbs({ crumbs }: { crumbs: readonly Crumb[] }) {
  return (
    <nav aria-label="Breadcrumb" className={styles.breadcrumbs}>
      <ol className={styles.breadcrumbList}>
        {crumbs.map((crumb, index) => (
          <li key={`${crumb.label}-${index}`}>
            {index > 0 && (
              <span aria-hidden="true" className={styles.breadcrumbSeparator}>
                {" / "}
              </span>
            )}

            {crumb.href === undefined ? (
              <span aria-current="page" className={styles.breadcrumbCurrent}>
                {crumb.label}
              </span>
            ) : (
              <Link href={crumb.href}>{crumb.label}</Link>
            )}
          </li>
        ))}
      </ol>
    </nav>
  );
}

/**
 * The top of every workspace screen: where you are, what this is, and what you
 * can do from here.
 *
 * Pure and synchronous — it takes strings, not a fetch — which is the split the
 * rest of this app already uses (`AppShell`, `HealthPanel`). The page does the
 * reading; this does the rendering.
 */
export function PageHeader({
  title,
  lede,
  crumbs,
  actions,
}: {
  title: string;
  lede?: ReactNode;
  crumbs?: readonly Crumb[];
  actions?: ReactNode;
}) {
  return (
    <header className={styles.header}>
      {crumbs !== undefined && crumbs.length > 0 && <Breadcrumbs crumbs={crumbs} />}

      <div className={styles.headerRow}>
        <h1 className={styles.title}>{title}</h1>
        {actions}
      </div>

      {lede !== undefined && <p className={styles.lede}>{lede}</p>}
    </header>
  );
}
