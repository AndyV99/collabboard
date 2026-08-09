"use client";

import { useRouter } from "next/navigation";
import { useState, useTransition } from "react";

import styles from "./workspace.module.css";

/**
 * "Try again" for a list that did not load.
 *
 * The data was fetched by a Server Component, so retrying means asking the
 * server to render the route again — `router.refresh()`, not a client-side
 * fetch. That keeps one fetching path for both the first attempt and the retry;
 * a button that fetched through `/api/proxy` instead would be a second
 * implementation of the same screen, able to succeed where the real one fails.
 *
 * `useTransition` is what makes the pending state truthful. `refresh()` returns
 * immediately and the re-render lands later, so without a transition the button
 * would report success the instant it was pressed.
 *
 * A `<button>` rather than a link to the same URL: a link is a navigation the
 * router can serve from its own cache, which is precisely the cache a retry
 * needs to bypass.
 */
export function RetryButton({ label = "Try again" }: { label?: string }) {
  const router = useRouter();
  const [pending, startTransition] = useTransition();

  // Cleared by the re-render when the retry succeeds; it only ever shows after
  // a retry that came back with the same failure still on screen.
  const [attempted, setAttempted] = useState(false);

  return (
    <div>
      <button
        className={styles.secondary}
        disabled={pending}
        onClick={() => {
          setAttempted(true);
          startTransition(() => {
            router.refresh();
          });
        }}
        type="button"
      >
        {pending ? "Trying…" : label}
      </button>

      {attempted && !pending && (
        <p className={styles.sectionNote} role="status">
          Still failing. The server may still be recovering.
        </p>
      )}
    </div>
  );
}
