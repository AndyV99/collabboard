"use client";

import type { ReactNode, RefObject } from "react";

import styles from "./auth.module.css";

/**
 * The block at the top of a form that says what went wrong.
 *
 * Two things make it work for a screen reader rather than only for a sighted
 * user, and both are easy to leave out:
 *
 * - **`role="alert"`**, so the text is announced when it appears without the
 *   user having to go looking for it. It is on a container that is only
 *   *rendered* when there is something to say — a permanently mounted empty
 *   alert region is announced inconsistently across screen readers, because
 *   what they watch for is a change inside a live region, and an element that
 *   springs into existence with content is the case they all handle.
 * - **`tabIndex={-1}` plus a ref**, so the form can move focus here after a
 *   failed submit. That is belt and braces on the announcement, and it is what
 *   puts the keyboard user next to the "problem" list rather than leaving them
 *   wherever the submit button was.
 *
 * The `<h2>` is deliberate: a heading makes the block a landmark a screen
 * reader user can jump back to, which a bare `<p>` does not.
 */
export type FormAlertProps = {
  title: string;
  children: ReactNode;
  /** `notice` for "this is not a failure, but read it" — a created account. */
  tone?: "error" | "notice";
  alertRef?: RefObject<HTMLDivElement | null>;
};

export function FormAlert({ title, children, tone = "error", alertRef }: FormAlertProps) {
  return (
    <div
      className={`${styles.alert}${tone === "notice" ? ` ${styles.alertNotice}` : ""}`}
      ref={alertRef}
      role="alert"
      tabIndex={-1}
    >
      <h2 className={styles.alertTitle}>{title}</h2>
      <div className={styles.alertBody}>{children}</div>
    </div>
  );
}

/**
 * The list of field problems inside a {@link FormAlert}.
 *
 * Each entry links to the input it is about. An in-page `#id` link moves focus
 * to that input in every current browser, so "which box do I fix" is one key
 * press away rather than a hunt down the form.
 */
export function FieldErrorList({
  errors,
}: {
  errors: readonly { field: string; message: string }[];
}) {
  return (
    <ul className={styles.alertList}>
      {errors.map((entry) => (
        <li key={entry.field}>
          <a href={`#${entry.field}`}>{entry.message}</a>
        </li>
      ))}
    </ul>
  );
}
