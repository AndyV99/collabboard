import type { Member } from "@/lib/api/types";
import { formatDate } from "@/lib/workspace/format";
import styles from "@/components/workspace/workspace.module.css";

/**
 * Everyone in the workspace.
 *
 * `GET /members` is tenant-scoped by the token's org claim, so this is exactly
 * the set of accounts that can see every project in the list one page up — which
 * is the sentence the members screen exists to make true rather than implied.
 *
 * The signed-in user's own row is marked. It is the only row whose role has a
 * consequence for what the page offers (see `lib/workspace/roles.ts`), so
 * "which one is me" needs to be answerable at a glance rather than by comparing
 * the address in the header.
 */
export function MemberList({
  members,
  viewerUserId,
}: {
  members: readonly Member[];
  viewerUserId: string;
}) {
  return (
    <ul className={styles.list}>
      {members.map((member) => {
        const joined = formatDate(member.joinedAt);
        const isViewer = member.userId === viewerUserId;

        return (
          <li className={styles.row} key={member.membershipId}>
            <span className={styles.rowName}>{member.displayName}</span>
            <span className={styles.rowDetail}>{member.email}</span>

            <span className={styles.rowSpacer} />

            {joined !== null && (
              <span className={styles.rowDetail}>Joined {joined}</span>
            )}

            {isViewer && (
              <span className={`${styles.badge} ${styles.badgeSelf}`}>You</span>
            )}

            <span className={styles.badge}>{member.role}</span>
          </li>
        );
      })}
    </ul>
  );
}
