import type { ReactNode } from "react";

import type { LoadFailure } from "@/lib/workspace/outcomes";
import { RetryButton } from "./retry-button";
import styles from "./workspace.module.css";

/**
 * The three things a list can be other than a list: empty, loading, or broken.
 *
 * All three are Server Components — pure, synchronous, and taking their content
 * as props — so each is a unit test rather than a state somebody has to
 * provoke against a real API. Between them they are why
 * `apps/web/README.md` can claim every list has an empty, loading and error
 * state without that being an aspiration.
 */

/**
 * What a list shows when there is genuinely nothing in it.
 *
 * An empty state is a screen, not a shrug, and on this app it is *the* screen: a
 * new account has no projects, so this is the first thing a reviewer of this
 * portfolio sees after signing up. So it gets a heading, an explanation of what
 * the thing is for, and the control that fixes it — not the words "No results".
 *
 * `children` is where the create form goes. Putting the form *inside* the empty
 * state rather than above the list means the one thing you can do is the one
 * thing on screen.
 */
export function EmptyState({
  title,
  children,
  body,
}: {
  title: string;
  body?: ReactNode;
  children?: ReactNode;
}) {
  return (
    <section className={styles.panel}>
      <h2 className={styles.panelTitle}>{title}</h2>
      {body !== undefined && <div className={styles.panelBody}>{body}</div>}
      {children}
    </section>
  );
}

/** A numbered explanation of how the pieces fit together. */
export function Steps({
  steps,
}: {
  steps: readonly { title: string; body: string }[];
}) {
  return (
    <ol className={styles.steps}>
      {steps.map((step, index) => (
        <li className={styles.step} key={step.title}>
          <span aria-hidden="true" className={styles.stepNumber}>
            {index + 1}
          </span>
          <span className={styles.stepBody}>
            <strong>{step.title}</strong> {step.body}
          </span>
        </li>
      ))}
    </ol>
  );
}

/**
 * What a list shows when it could not be read.
 *
 * `role="alert"` is deliberately *not* here. This renders as part of the first
 * paint, not in response to something the user did, and an alert is for the
 * latter — a region that announces itself while a page is still being read
 * interrupts the reading order of a page the user has only just arrived at. The
 * heading does the job: it is in the document outline and a screen reader user
 * meets it in sequence.
 *
 * The retry is offered only when trying again could plausibly work.
 * {@link LoadFailure} carries that as data rather than leaving each screen to
 * guess, so "Try again" never appears next to "your session has ended".
 */
export function LoadError({ failure }: { failure: LoadFailure }) {
  return (
    <section className={`${styles.panel} ${styles.panelDanger}`}>
      <h2 className={styles.panelTitle}>{failure.title}</h2>
      <p className={styles.panelBody}>{failure.message}</p>
      {failure.retryable && <RetryButton />}
    </section>
  );
}

/**
 * The placeholder a `loading.tsx` renders while a page's data is in flight.
 *
 * It mirrors the shape of what is coming rather than showing a spinner, so the
 * layout does not jump when the real content arrives.
 *
 * The boxes are `aria-hidden` and the only thing announced is one `role="status"`
 * line naming what is loading. A screen reader user should hear "Loading
 * projects…" once, not be read a dozen empty list items.
 */
export function ListSkeleton({ rows = 3, label }: { rows?: number; label: string }) {
  return (
    <section className={styles.section}>
      <p className={styles.sectionNote} role="status">
        {label}
      </p>

      <ul aria-hidden="true" className={`${styles.list} ${styles.listGrid}`}>
        {Array.from({ length: rows }, (_, index) => (
          <li className={styles.skeleton} key={index} />
        ))}
      </ul>
    </section>
  );
}
