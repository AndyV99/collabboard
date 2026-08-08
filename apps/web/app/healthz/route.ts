import { connection } from "next/server";

import { logEvent } from "@/lib/log";
import { evaluateReadiness } from "@/lib/readiness";

/**
 * `GET /healthz` — the web tier's readiness signal, for the ALB target group
 * and the image's `HEALTHCHECK`.
 *
 * The policy (what "ready" means, and why the API's reachability is not part
 * of it) lives in `lib/readiness.ts`. This file is the transport: run at
 * request time, log the suppressed detail, serve the verdict uncached.
 *
 * Named `/healthz` to match the Go API's endpoint, so "check the health of
 * component X" is one path regardless of which service X is. There is no
 * separate liveness route: the web tier is stateless with no warm-up, so
 * "would a restart help?" and "should traffic arrive?" have the same answer,
 * and two routes that can never disagree are two routes to keep in sync.
 */
export async function GET() {
  // Health is a fact about right now. Without this the handler reads only
  // `process.env`, which Next does not treat as runtime data, so it is a valid
  // prerender candidate — and a prerendered readiness route would freeze the
  // build machine's answer into the image and report `source: "default"`
  // forever. That is the same failure #16 removed from the page, arriving
  // through a different door. Keep this call.
  await connection();

  const { httpStatus, body, detail } = evaluateReadiness();

  if (detail !== null) {
    // The full message names the offending value, so it goes here and not into
    // the response body (#31). In practice `instrumentation.ts` already exited
    // the process on this, so reaching it means the boot check regressed or was
    // bypassed — worth a line either way.
    logEvent("error", "readiness check failed", detail);
  }

  return Response.json(body, {
    status: httpStatus,
    headers: {
      // A cached health check is not a health check. Belt and braces against a
      // CDN or proxy in front of the ALB deciding a 200 is reusable.
      "cache-control": "no-store",
    },
  });
}
