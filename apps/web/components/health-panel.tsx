import type { HealthProbe } from "@/lib/health";
import { STATUS_OK } from "@/lib/health";
import styles from "./health-panel.module.css";

/**
 * Renders the result of an API health probe.
 *
 * Deliberately a pure, synchronous component that takes the already-resolved
 * probe as a prop: the fetching lives in the page (a Server Component), which
 * keeps every rendering branch — healthy, degraded, unreachable — directly
 * testable without a network or an async component renderer.
 */
export function HealthPanel({ probe }: { probe: HealthProbe }) {
  return (
    <section className={styles.panel} aria-labelledby="api-health-title">
      <div className={styles.header}>
        <h2 className={styles.title} id="api-health-title">
          API health
        </h2>
        <Badge probe={probe} />
      </div>

      <div className={styles.meta}>
        <span>
          Endpoint: <code>{probe.url}</code>
        </span>
        {probe.outcome !== "unreachable" && (
          <span>HTTP {probe.httpStatus}</span>
        )}
      </div>

      <Body probe={probe} />
    </section>
  );
}

function Badge({ probe }: { probe: HealthProbe }) {
  if (probe.outcome !== "reachable") {
    return (
      <span className={`${styles.badge} ${styles.badgeDown}`}>
        No response
      </span>
    );
  }

  const healthy = probe.health.status === STATUS_OK;

  return (
    <span
      className={`${styles.badge} ${healthy ? styles.badgeOk : styles.badgeDegraded}`}
    >
      {healthy ? "Healthy" : "Degraded"}
    </span>
  );
}

function Body({ probe }: { probe: HealthProbe }) {
  if (probe.outcome === "unreachable") {
    return (
      <p className={styles.explanation}>
        Could not reach the API. Start it with{" "}
        <code>go run ./cmd/api</code> from <code>apps/api</code>, or point{" "}
        <code>API_URL</code> at a running instance. Details: {probe.error}
      </p>
    );
  }

  if (probe.outcome === "malformed") {
    return (
      <p className={styles.explanation}>
        The API answered, but the response did not look like{" "}
        <code>/healthz</code>. Check that <code>API_URL</code> points at the
        CollabBoard API. Details: {probe.error}
      </p>
    );
  }

  const components = Object.entries(probe.health.components).sort(([a], [b]) =>
    a.localeCompare(b),
  );

  return (
    <>
      <p className={styles.explanation}>
        Reported status: <strong>{probe.health.status}</strong>
      </p>

      {components.length === 0 ? (
        <p className={styles.explanation}>
          The API did not report any dependencies.
        </p>
      ) : (
        <ul className={styles.components}>
          {components.map(([name, component]) => (
            <li className={styles.component} key={name}>
              <span className={styles.componentName}>{name}</span>
              <span className={styles.componentStatus}>
                {component.status}
              </span>
              {component.error && (
                <span className={styles.componentError}>{component.error}</span>
              )}
            </li>
          ))}
        </ul>
      )}
    </>
  );
}
