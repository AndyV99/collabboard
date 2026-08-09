import type { Metadata } from "next";

import { requireSession } from "@/lib/session/require";
import styles from "./page.module.css";

export const metadata: Metadata = {
  title: "Your workspace · CollabBoard",
};

/**
 * Where signing in lands you.
 *
 * A placeholder, and deliberately a thin one: projects, boards and cards are
 * issues #62 and #63, and building a stand-in for them here would be work
 * thrown away plus a second design to reconcile. What it does prove is the part
 * this issue is about — that a Server Component behind the protected layout can
 * read the session and render it with no client-side fetch and no loading state.
 *
 * `requireSession()` runs in the layout too. Calling it again costs a cookie
 * read and no request, and it means this page states its own requirement rather
 * than inheriting one silently.
 */
export default async function WorkspacePage() {
  const session = await requireSession();

  return (
    <div className={styles.page}>
      <h1 className={styles.heading}>{session.organization.name}</h1>

      <p className={styles.body}>
        You are signed in as {session.organization.role || "a member"} of this
        workspace. Everything you create here — projects, boards, cards — belongs
        to it, and only its members can see it.
      </p>

      <p className={styles.body}>Still to come:</p>

      <ul className={styles.next}>
        <li>Projects and boards (issue #62)</li>
        <li>Cards, with drag and drop (issue #63)</li>
        <li>Live updates over the WebSocket (issue #9)</li>
      </ul>
    </div>
  );
}
