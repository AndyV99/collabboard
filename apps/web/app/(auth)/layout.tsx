import type { ReactNode } from "react";

import styles from "@/components/auth/auth.module.css";

/**
 * The frame around the signed-out screens.
 *
 * Layout only — no session check. Deciding what an *unauthenticated* visitor
 * may see is not this group's job; it is the whole point of this group that they
 * may see it. The pages themselves send an already-signed-in visitor onward,
 * because that decision needs the `next` parameter and a layout is not given
 * one.
 */
export default function AuthLayout({ children }: { children: ReactNode }) {
  return (
    <main className={styles.shell}>
      <div className={styles.card}>{children}</div>
    </main>
  );
}
