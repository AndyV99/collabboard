"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";

import { signOut } from "@/lib/api/browser";
import { SIGN_IN_PATH } from "@/lib/auth/routes";
import styles from "./app-shell.module.css";

/**
 * Ends the session and sends the user to the sign-in screen.
 *
 * A `<button>` inside no form, not a link. Signing out changes state on the
 * server, and a GET that changes state is a state change a prefetcher, a
 * crawler, or a link preview can perform on the user's behalf — Next's own
 * `<Link>` prefetching would be enough to do it.
 *
 * `signOut()` never rejects and never reports failure: `app/api/auth/logout`
 * clears the cookies whatever the API says, including when it cannot be reached
 * at all, because a logout that leaves a live cookie behind because a network
 * call failed is a logout that did not happen after the user was told it did.
 * So this navigates unconditionally.
 *
 * `refresh()` after `replace()` is what discards the React Server Component
 * cache. Without it the previously rendered shell — with a name and a workspace
 * in it — can be served from the client-side cache to somebody who has just
 * signed out.
 */
export function SignOutButton() {
  const router = useRouter();
  const [pending, setPending] = useState(false);

  async function handleClick() {
    if (pending) {
      return;
    }

    setPending(true);

    await signOut();

    router.replace(SIGN_IN_PATH);
    router.refresh();
  }

  return (
    <button
      className={styles.signOut}
      disabled={pending}
      onClick={handleClick}
      type="button"
    >
      {pending ? "Signing out…" : "Sign out"}
    </button>
  );
}
