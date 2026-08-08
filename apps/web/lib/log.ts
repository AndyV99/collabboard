/**
 * Minimal structured logger for the server side of the web app.
 *
 * The Observability standard asks for one JSON line per meaningful event with
 * consistent field names (`service`, `level`, `message`, plus context). The Go
 * API already emits that shape via slog; this keeps the web app's server logs
 * greppable alongside it without pulling in a logging dependency for what is
 * currently a handful of call sites.
 */

export type LogLevel = "info" | "warn" | "error";

export const SERVICE_NAME = "collabboard-web";

export function logEvent(
  level: LogLevel,
  message: string,
  fields: Record<string, unknown> = {},
): void {
  const line = JSON.stringify({
    time: new Date().toISOString(),
    level,
    service: SERVICE_NAME,
    message,
    ...fields,
  });

  if (level === "error") {
    console.error(line);
    return;
  }

  if (level === "warn") {
    console.warn(line);
    return;
  }

  console.info(line);
}
