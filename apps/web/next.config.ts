import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  /**
   * Emit `.next/standalone`: a self-contained server plus only the traced
   * subset of `node_modules` each route actually reaches.
   *
   * This is what lets `Dockerfile`'s runtime stage ship no `node_modules` of
   * its own and no source tree. Without it a runtime image has to carry the
   * full install (dev dependencies included, unless a second `npm ci --omit=dev`
   * is bolted on) purely so `next start` can resolve its own imports.
   *
   * It changes the start command: the image runs `node server.js` from the
   * standalone directory, not `next start`.
   *
   * `next start` still serves a build correctly, but it now prints
   * `"next start" does not work with "output: standalone"`. That warning is
   * expected here and is not a misconfiguration — the local loop is
   * `npm run dev`, and the deployed loop is the image. Read it as "you are not
   * running what production runs", which is exactly true.
   */
  output: "standalone",
};

export default nextConfig;
