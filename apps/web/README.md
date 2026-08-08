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

`npm start` (`next start`) still serves a build, but prints
`"next start" does not work with "output: standalone"`. That warning is expected
(see [Container image](#container-image)) — use `npm run dev` locally, or the
image for anything production-shaped.

## Container image

`Dockerfile` builds the deployable artifact. The **context is the repo root**,
so build from there, not from this directory:

```bash
# from the repo root
docker build -f apps/web/Dockerfile -t collabboard-web:dev .

# API on the host (the compose stack + `go run ./cmd/api` from apps/api)
docker run --rm -p 3000:3000 \
  --add-host host.docker.internal:host-gateway \
  -e API_URL=http://host.docker.internal:8080 \
  collabboard-web:dev

# or against an API container, once apps/api has an image, on a shared network
docker run --rm -p 3000:3000 --network <that-network> \
  -e API_URL=http://<api-container-name>:8080 \
  collabboard-web:dev
```

Then `http://localhost:3000` for the page and `http://localhost:3000/healthz`
for readiness.

**`API_URL` is a run-time flag, never a build-time one.** There is no
`--build-arg` for it and no `ENV API_URL` in the image, on purpose: baking it in
would undo the whole point of resolving it at request time. The same tag runs
against a different API by changing nothing but the environment:

```bash
docker run -d -p 3001:3000 -e API_URL=http://api.staging.internal:8080 collabboard-web:dev
docker run -d -p 3002:3000 -e API_URL=http://api.prod.internal:8080    collabboard-web:dev
```

An image run with no `API_URL` at all still starts — it falls back to
`http://localhost:8080`, which inside a container is the container itself. That
is a misconfiguration, not a design, and `/healthz` reports it as
`"source": "default"`.

### How it is built

Three stages, so the runtime image carries neither the toolchain nor the full
dependency tree:

1. `deps` — `npm ci` from the lockfile alone, so a source edit reuses the layer.
2. `builder` — `next build`, which with `output: "standalone"` emits a
   `server.js` plus only the `node_modules` the routes actually reach.
3. `runner` — copies that standalone tree, the static assets, and nothing else.

The difference is the point: the full install is ~667 MB across 346 packages;
the traced subset in the image is ~64 MB across 12, with no dev dependencies,
no `next` CLI and no source tree. The image is ~325 MB, of which ~232 MB is the
`node:22-alpine` base.

Other properties worth knowing before changing the file:

- **Runs as `node` (uid 1000), not root.** `docker run --rm collabboard-web:dev id`
  → `uid=1000(node) gid=1000(node)`.
- **npm, yarn and corepack are removed from the runtime stage.** The container
  runs `node server.js`; the package managers are only an attack surface and
  their vendored dependencies were the *only* source of HIGH/CRITICAL findings
  in this image. With them gone,
  `trivy image --severity HIGH,CRITICAL collabboard-web:dev` reports none.
- **`HOSTNAME=0.0.0.0`** is load-bearing. Standalone's `server.js` binds
  `localhost` by default, which is unreachable from outside the container.
- **`HEALTHCHECK`** polls `/healthz` with `node -e`, so it needs no `curl` or
  `wget` in the image and exercises HTTP rather than a bare TCP connect.
- **Node is PID 1** via the exec-form `CMD`, and Next registers a `SIGTERM`
  handler, so `docker stop` (and an ECS task drain) exits in well under a
  second instead of waiting out the 10s kill timer.
- **Base image tags are pinned to a patch version**, matching how CI pins its
  toolchains: a base image refresh should arrive as a reviewed commit.
- The repo-root **`.dockerignore`** keeps the context at ~340 kB against a
  668 MB working tree, and — more importantly — stops the host's
  `node_modules`/`.next` from shadowing the ones built inside the image.

## Readiness: `GET /healthz`

The web app's own health signal, for a load balancer target group and for the
image's `HEALTHCHECK`.

```console
$ curl -s localhost:3000/healthz
{"status":"ok","service":"collabboard-web","components":{"api_url_config":{"status":"ok","source":"env"}}}
```

`200` when ready, `503` when not, always `cache-control: no-store`.

**Ready means the web app can serve a request and its own configuration
resolved.** Answering at all proves Next booted and can route — which a TCP
port check does not, since a process wedged after `listen()` still passes that.
The `api_url_config` component proves the other half, the half `/` cannot
report: `/` returns 200 with a degraded panel whether `API_URL` is sound or not.

**Ready deliberately does not mean the API is reachable.** This route never
calls the API. Rendering a degraded panel during an API outage is designed
behaviour, and it can only be served by a task the load balancer still routes
to — health-checking the API here would deregister every web task the moment
the API blinked, and would point a per-task, per-few-seconds traffic source at
it besides. The API's health is its own target group's business.

`source` (`env` vs `default`) is in the payload because the likeliest real
misconfiguration is a task definition that never set `API_URL`, leaving the
container quietly pointed at its own localhost. The **resolved URL is not** in
the payload: this route is unauthenticated, the URL is internal topology, and
issue #31 applies to it. The value is already in the logs — `instrumentation.ts`
emits one structured line per boot naming it. Same split when resolution fails:
the message quoting the bad value is logged, the response body says only
`"status": "unavailable"`.

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
app/                 routes (App Router). page.tsx is the health placeholder,
                     healthz/route.ts is the readiness signal.
components/          presentational components, kept pure so they are unit testable
lib/                 API client helpers, the readiness policy, the structured logger
instrumentation.ts   server startup hook: validates + logs the resolved API_URL
Dockerfile           multi-stage build of the deployable image (context: repo root)
__tests__/           vitest + React Testing Library
```

The data fetch lives in the Server Component and the rendering lives in a pure
component that takes the resolved result as a prop — that split is what makes
the healthy, degraded, and unreachable branches testable without a network.
