import { connection } from "next/server";

import { HealthPanel } from "@/components/health-panel";
import { probeHealth } from "@/lib/health";
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
  await connection();

  const probe = await probeHealth();

  return (
    <div className={styles.page}>
      <main className={styles.main}>
        <header className={styles.intro}>
          <h1 className={styles.heading}>CollabBoard</h1>
          <p className={styles.subheading}>
            App shell only. The panel below is server-rendered from the Go
            API&rsquo;s <code>/healthz</code> endpoint — board UI lands in a
            later issue.
          </p>
        </header>

        <HealthPanel probe={probe} />
      </main>
    </div>
  );
}
