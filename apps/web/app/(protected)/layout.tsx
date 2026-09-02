import type { ReactNode } from "react";

import { AppShell } from "@/components/app-shell";
import { requireSession } from "@/lib/session/require";
import { loadCurrentUser } from "@/lib/session/me";

/**
 * Route protection, in one place, for everything under this group.
 *
 * `requireSession()` redirects an unauthenticated visitor to the sign-in screen
 * with the page they wanted attached, and returns the session otherwise. Doing
 * it in the layout rather than in each page is the point: a page added under
 * this group is protected because of where its file is, not because somebody
 * remembered to add a check to it. Boards and cards land here in #62 and #63.
 *
 * A layout does run again on client-side navigations within the group, so this
 * is not a one-time gate that a later navigation slips past.
 */
export default async function ProtectedLayout({ children }: { children: ReactNode }) {
  const session = await requireSession();
  const viewer = await loadCurrentUser();

  return (
    <AppShell organization={session.organization} viewer={viewer}>
      {children}
    </AppShell>
  );
}
