# apps/web

Next.js (App Router, React Server Components) front end for CollabBoard.

Currently the app shell only: a single placeholder route that server-side
fetches `GET /healthz` from the Go API and renders its status. Board UI, auth,
and the WebSocket client land in later issues.

## Commands

Run from this directory:

```bash
npm install
npm run dev        # http://localhost:3000
npm run lint
npm test           # vitest run
npm run typecheck  # tsc --noEmit
npm run build
```

## Configuration

| Variable              | Default                 | Purpose                     |
| --------------------- | ----------------------- | --------------------------- |
| `NEXT_PUBLIC_API_URL` | `http://localhost:8080` | Base URL of the Go API      |

Copy `.env.example` to `.env.local` to override. `NEXT_PUBLIC_*` values are
inlined at build time, so a change only takes effect after a rebuild.

The API is expected to be running (`go run ./cmd/api` from `apps/api`, with the
root `docker compose up -d` stack for Postgres and Redis). If it is not, the
page renders a degraded state rather than failing.

## Layout

```
app/          routes (App Router). page.tsx is the health placeholder.
components/   presentational components, kept pure so they are unit testable
lib/          API client helpers and the structured logger
__tests__/    vitest + React Testing Library
```

The data fetch lives in the Server Component and the rendering lives in a pure
component that takes the resolved result as a prop — that split is what makes
the healthy, degraded, and unreachable branches testable without a network.
