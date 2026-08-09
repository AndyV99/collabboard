import Link from "next/link";
import { connection } from "next/server";

import { HealthPanel } from "@/components/health-panel";
import { DEFAULT_SIGNED_IN_PATH, SIGN_IN_PATH, SIGN_UP_PATH } from "@/lib/auth/routes";
import { probeHealth } from "@/lib/health";
import { getRenderSession } from "@/lib/session/server";
import styles from "./page.module.css";

/**
 * Placeholder landing page for the app shell.
 *
 * It exists to prove the web-to-API path end to end: this is a Server
 * Component, so the `/healthz` request happens on the server at request time
 * and the browser only ever receives rendered HTML.
 */
export default async function Home() {
  // Health is a live fact about right now, so the page must never be captured
  // into a prerender at build time. `connection()` is the Next 16 way to say
  // that (it replaces `unstable_noStore` and survives Cache Components).
  //
  // It is also what makes `API_URL` a runtime value: env reads are only
  // request-time reads on a dynamically rendered page. If this page were ever
  // statically optimised, the base URL would be resolved by the build worker
  // and frozen into the prerendered HTML — the exact failure #16 fixed. The
  // build output must keep showing `ƒ /`, not `○ /`.
  await connection();

  // The public page, so it reads the session rather than requiring one: a
  // visitor with no session sees the way in, and one with a session sees the
  // way back. Neither is a redirect — this route belongs to nobody.
  const [probe, session] = await Promise.all([probeHealth(), getRenderSession()]);

  return (
    <div className={styles.page}>
      <main className={styles.main}>
        <header className={styles.intro}>
          <h1 className={styles.heading}>CollabBoard</h1>
          <p className={styles.subheading}>
            Multi-tenant, real-time Kanban. Sign in to reach your workspace; the
            panel below is server-rendered from the Go API&rsquo;s{" "}
            <code>/healthz</code> endpoint, and board UI lands in a later issue.
          </p>

          <div className={styles.actions}>
            {session === null ? (
              <>
                <Link className={styles.primary} href={SIGN_IN_PATH}>
                  Sign in
                </Link>
                <Link className={styles.secondary} href={SIGN_UP_PATH}>
                  Create an account
                </Link>
              </>
            ) : (
              <Link className={styles.primary} href={DEFAULT_SIGNED_IN_PATH}>
                Open {session.organization.name}
              </Link>
            )}
          </div>
        </header>

        <HealthPanel probe={probe} />
      </main>
    </div>
  );
}
