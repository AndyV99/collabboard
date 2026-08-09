import type { ReactNode } from "react";

import type { Organization } from "@/lib/api/types";
import type { Viewer } from "@/lib/session/viewer";
import { SignOutButton } from "./sign-out-button";
import styles from "./app-shell.module.css";

/**
 * The frame every signed-in screen renders inside.
 *
 * Pure and synchronous, taking the already-resolved session as props — the same
 * split `HealthPanel` uses, and for the same reason: every branch (a viewer, no
 * viewer, a role, no role) is directly testable without a network or an async
 * component renderer. The fetching lives in the layout.
 *
 * `viewer` is null when the API could not tell us who we are. The shell still
 * renders: the workspace comes from the session cookie and needs no request, so
 * an API outage costs the user a name in the corner rather than the page.
 */
export function AppShell({
  organization,
  viewer,
  children,
}: {
  organization: Organization;
  viewer: Viewer | null;
  children: ReactNode;
}) {
  return (
    <div className={styles.shell}>
      <header className={styles.header}>
        <span className={styles.brand}>CollabBoard</span>

        <div className={styles.workspace}>
          <span className={styles.workspaceLabel} id="workspace-label">
            Workspace
          </span>
          <span aria-labelledby="workspace-label" className={styles.workspaceName}>
            {organization.name}
          </span>
          {organization.role !== "" && (
            <span className={styles.role}>{organization.role}</span>
          )}
        </div>

        <div className={styles.spacer} />

        <div className={styles.viewer}>
          {viewer === null ? (
            <span className={styles.viewerDetail}>Signed in</span>
          ) : (
            <>
              <span className={styles.viewerName}>{viewer.displayName}</span>
              <span className={styles.viewerDetail}>{viewer.email}</span>
            </>
          )}
        </div>

        <SignOutButton />
      </header>

      <main className={styles.content}>{children}</main>
    </div>
  );
}
