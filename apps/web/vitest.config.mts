import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [react()],
  // Resolves the `@/*` alias from tsconfig.json natively — Vite 7 supersedes
  // the vite-tsconfig-paths plugin the Next.js docs still recommend.
  resolve: { tsconfigPaths: true },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./vitest.setup.ts"],
    include: ["__tests__/**/*.test.{ts,tsx}"],
    env: {
      /*
       * The suite runs in a zone that is not UTC, deliberately.
       *
       * Almost everything this app renders is formatted in a fixed UTC — see
       * `lib/workspace/format.ts` — and a suite running in UTC cannot tell code
       * that fixed the zone from code that happened to be right because the
       * machine was. Since #157 one thing genuinely is rendered in the
       * *reader's* zone (a card's due date), and its whole failure mode is
       * being off by a day for anyone outside UTC.
       *
       * Pacific/Auckland is chosen for being awkward: +12/+13, on the far side
       * of the date line, so an instant late in a UTC day belongs to the next
       * local day, and its DST changeover is in a different month from the
       * northern hemisphere's.
       */
      TZ: "Pacific/Auckland",
    },
  },
});
