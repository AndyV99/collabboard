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

| Variable  | Scope       | Default                 | Purpose                |
| --------- | ----------- | ----------------------- | ---------------------- |
| `API_URL` | server-only | `http://localhost:8080` | Base URL of the Go API |

Copy `.env.example` to `.env.local` to override locally. `API_URL` is resolved
from the process environment **at request time**, so the same built image can be
promoted dev → staging → prod with a different value each time and no rebuild.

Rules, enforced in `lib/api.ts`:

- **Unset** → `http://localhost:8080`. Local dev needs no `.env.local`.
- **Set** → must be an absolute `http`/`https` URL. A trailing slash is
  stripped; a path prefix (`https://example.com/api`) is allowed; a query
  string or fragment is not.
- **Set but malformed** — empty, no scheme, an unsubstituted `${...}`
  placeholder — throws `ApiUrlConfigError`. `instrumentation.ts` calls the
  resolver in `register()`, so a misconfigured container fails at startup with
  a message naming the variable and the bad value, instead of serving 500s that
  look like an API outage. That same hook logs one structured line naming the
  resolved URL, which is how you tell from logs which API a task is talking to.

The API is expected to be running (`go run ./cmd/api` from `apps/api`, with the
root `docker compose up -d` stack for Postgres and Redis). If it is not, the
page renders a degraded state rather than failing.

### Why not `NEXT_PUBLIC_API_URL`

`next build` replaces every `process.env.NEXT_PUBLIC_*` reference with a literal
value, so a public variable is frozen into the artifact: the image that passed
staging would not be the image that runs in production. The health fetch happens
in a Server Component, so the browser never needed the value anyway.

**If a future Client Component needs to reach the API**, do not reintroduce a
`NEXT_PUBLIC_` variable. Two options, in order of preference:

1. **Proxy through a Route Handler** on the web origin (`app/api/.../route.ts`).
   The API origin stays server-side, requests are same-origin so there is no
   CORS config, and session cookies stay first-party.
2. **Pass the resolved value down as a prop** from a Server Component to the
   Client Component. It is still resolved at request time, so it stays
   runtime-configurable — the browser just receives it in the RSC payload.

Option 2 is the answer for the WebSocket client (issue #9), which a Route
Handler cannot proxy. Either way the variable stays server-only and the
build-once/promote model holds.

## Layout

```
app/                 routes (App Router). page.tsx is the health placeholder.
components/          presentational components, kept pure so they are unit testable
lib/                 API client helpers and the structured logger
instrumentation.ts   server startup hook: validates + logs the resolved API_URL
__tests__/           vitest + React Testing Library
```

The data fetch lives in the Server Component and the rendering lives in a pure
component that takes the resolved result as a prop — that split is what makes
the healthy, degraded, and unreachable branches testable without a network.
