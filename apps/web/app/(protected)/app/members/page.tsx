import type { Metadata } from "next";
import Link from "next/link";

import { listMembers } from "@/lib/api/endpoints";
import { serverApi } from "@/lib/api/server";
import { requireSession } from "@/lib/session/require";
import { describeLoadFailure } from "@/lib/workspace/outcomes";
import { canAddMembers, grantableRoles, roleInOrganization } from "@/lib/workspace/roles";
import { WORKSPACE_PATH } from "@/lib/workspace/routes";
import { AddMemberForm } from "@/components/members/add-member-form";
import { MemberList } from "@/components/members/member-list";
import { PageHeader } from "@/components/workspace/page-header";
import { EmptyState, LoadError } from "@/components/workspace/states";
import styles from "@/components/workspace/workspace.module.css";

export const metadata: Metadata = {
  title: "People · CollabBoard",
};

/**
 * Who is in this workspace, and — for an owner or an admin — how to add someone.
 *
 * # The caller's role comes from this page's own data
 *
 * `GET /members` returns every membership row in the tenant, including the
 * caller's, and that row is precisely what `AddMember` reads when it decides
 * whether the addition is allowed. So the page derives the role from the list it
 * already has rather than from `session.organization.role`, which is a token
 * claim minted at login and stale for up to an access-token lifetime after a
 * promotion or a demotion. One request, and the answer the server will give.
 *
 * A caller who does not appear in the list gets nothing offered. That is what
 * issue #34's half-registered account looks like from here, and what a removed
 * member's still-valid token looks like until it expires — in both cases the
 * form would 403, so it is not shown.
 *
 * # A `member` is told, not shown a button that fails
 *
 * ADR 0008: only an owner or an admin may add, because `member` is the role
 * every added account gets and a member who could add would be enough to grow an
 * organization without limit. The screen states the rule instead of offering the
 * action.
 */
export default async function MembersPage() {
  const session = await requireSession();
  const result = await serverApi(listMembers());

  const crumbs = [{ href: WORKSPACE_PATH, label: "Projects" }, { label: "People" }];

  if (!result.ok) {
    return (
      <div className={styles.page}>
        <PageHeader crumbs={crumbs} title="People" />
        <LoadError failure={describeLoadFailure(result.error, "members")} />
      </div>
    );
  }

  const members = result.data;
  const role = roleInOrganization(members, session.userId);
  const canAdd = canAddMembers(role);
  const roles = grantableRoles(role);
  const alone = members.length <= 1;

  return (
    <div className={styles.page}>
      <PageHeader
        crumbs={crumbs}
        lede={
          <>
            Everyone here can see every project in{" "}
            <strong>{session.organization.name}</strong>. Membership is the only access
            control — there are no per-project permissions.
          </>
        }
        title="People"
      />

      <section className={styles.section}>
        <h2 className={styles.sectionHeading}>
          {members.length === 1 ? "1 person" : `${members.length} people`}
        </h2>

        <MemberList members={members} viewerUserId={session.userId} />
      </section>

      {canAdd ? (
        <section className={styles.section}>
          <h2 className={styles.sectionHeading}>Add someone</h2>

          {alone && (
            <EmptyState
              body={
                <p>
                  You are the only person in this workspace. Adding a colleague is what
                  makes the boards worth sharing — and it is the second browser you need
                  to watch a card move in real time.
                </p>
              }
              title="It is just you so far"
            />
          )}

          <div className={styles.panel}>
            <AddMemberForm roles={roles} />
          </div>
        </section>
      ) : (
        <section className={styles.section}>
          <h2 className={styles.sectionHeading}>Adding people</h2>

          <div className={styles.panel}>
            <p className={styles.panelBody}>
              {role === null
                ? "This account does not currently hold a membership in this workspace, so it cannot add anyone. That happens when a membership is revoked while a session is open — the access token still names this organization for the rest of its lifetime. Use Sign out at the top of the page, then sign in again."
                : "Only an owner or an admin can add someone to this workspace. Ask one of them — they are listed above with their role."}
            </p>
          </div>
        </section>
      )}

      <p className={styles.sectionNote}>
        <Link href={WORKSPACE_PATH}>Back to all projects</Link>
      </p>
    </div>
  );
}
