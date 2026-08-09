# apps/web

Next.js (App Router, React Server Components) front end for CollabBoard.

What exists: a public landing page that server-side fetches `GET /healthz` from
the Go API, the [authentication screens](#the-authentication-screens) (register,
sign in, sign out, route protection), and a signed-in shell. Project, board and
card UI is issues #62 and #63; the WebSocket client is #9.

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

Option 1 is what `app/api/proxy/[...path]/route.ts` implements — see
[Sessions and the API client](#sessions-and-the-api-client).

## Sessions and the API client

**The browser never holds a token.** `POST /api/v1/auth/login` returns an access
token and a 14-day refresh token in its body; a Route Handler on *this* origin
reads that body and puts both into `httpOnly` cookies. Client JavaScript cannot
read them, and nothing this app returns to a browser contains one.

The reasoning, the options rejected, and the honest limits are in
[ADR 0007](../../docs/adr/0007-web-session-storage.md). This section is the rule
every screen follows.

### The boundary, in one table

| You are writing | Use | Can it refresh? |
| --- | --- | --- |
| A Server Component | `serverApi` from `lib/api/server.ts` | No — `proxy.ts` already did |
| A Server Action or Route Handler | `mutableServerApi` from `lib/api/server.ts` | Yes |
| A Client Component | `browserApi()` from `lib/api/browser.ts` | Yes, via `/api/auth/refresh` |

Reading the session directly follows the same split: `getRenderSession()` in a
Server Component, `getServerSession()` anywhere a client reaches you directly.
The difference is that the render one trusts the header `proxy.ts` uses to pass a
just-refreshed token forward, and that header is unsigned — so a Route Handler,
which a client can call, must not consult it.

All three take the same values from `lib/api/endpoints.ts` and return the same
`ApiResult<T>`, so a call reads identically either side of the line.

**Prefer a Server Component.** It has the session already, costs no round trip,
and ships no fetching code to the browser. Fetch there and pass plain data down
as props. Reach for a Client Component when the interaction demands it — drag and
drop, live updates — not to fetch.

**Never import `lib/session/*` or `lib/api/server.ts` from a Client Component.**
They import `next/headers`, so the build fails rather than shipping them. That
failure is the boundary being enforced, not a problem to work around.

### Why a Server Component cannot refresh

`cookies().set()` is illegal during rendering — the response has already begun.
The API *rotates* refresh tokens, so a refresh whose successor cannot be stored
spends the session's credential and throws the replacement away: one render would
cost the user their session. `serverApi` therefore returns `unauthorized` instead,
and `proxy.ts` — Next 16's renamed middleware, which runs before the render and
*can* set cookies — is what keeps the token fresh. By the time a page renders the
access token is fresh or absent, never expired-but-renewable.

`proxy.ts` is excluded from `/api/*`: those handlers refresh for themselves, and
two refreshers on one request would spend the same token twice.

### Errors are values

```ts
const result = await serverApi(endpoints.getBoard(boardId));

if (!result.ok) {
  switch (result.error.kind) {
    case "not_found":    return <BoardMissing />;   // also "another tenant's"
    case "forbidden":    return <NoAccess />;
    case "conflict":     return <Stale />;          // a stale drag, usually
    case "rate_limited": return <TryLater seconds={result.error.retryAfterSeconds} />;
    case "unauthorized": return <SignedOut />;
    default:             return <Unavailable />;    // server_error | network | malformed
  }
}
```

Nothing throws for a failure the server described. `result.error.message` is safe
to show: it is the API's own message or a local default, never a stack trace and
never the body of a 5xx.

Note `not_found`: the API answers 404 for another tenant's object on purpose, so
it must not be read as "exists but is not yours".

### Signing out is a state, not a navigation

Nothing in this layer redirects. A failed refresh clears the cookies and every
call returns `unauthorized`; the browser client reports it through
`onSignedOut(...)`. Where to send an unauthenticated visitor is the screen's
decision, made once, in one place — a redirect issued from inside a fetch helper
runs on every path including the sign-in page, which is how that becomes a loop.

### Adding an endpoint

1. Add the type and its parser to `lib/api/types.ts`. Parsers are hand-written on
   purpose: the API publishes no OpenAPI document, and a generated client's types
   are a cast that would let a misconfigured `API_URL` render `undefined`.
2. Add the `Endpoint` to `lib/api/endpoints.ts`, path relative to `/api/v1`.
3. If a Client Component needs it, check its first path segment is in
   `PROXIED_ROOTS` (`lib/api/proxy-route.ts`). That list is an allowlist, and
   `auth` is deliberately absent — `/api/proxy/auth/login` would hand a browser a
   refresh token. Do not relax the dot-segment refusal in `proxyTarget` to make a
   path work: `..` survives `encodeURIComponent` and is normalised away by
   `fetch`, which is how an allowed root reaches `/auth/*` anyway.

## The authentication screens

Four routes, and one rule for each of them:

| Route | What it is | Session |
| --- | --- | --- |
| `/` | Public landing + API health | Read, never required |
| `/login` | Sign in | Redirects **away** if you have one |
| `/register` | Create an account and its workspace | Redirects **away** if you have one |
| `/app` | The signed-in placeholder | Required |

`app/(auth)/` and `app/(protected)/` are route groups, so the URLs above have no
segment for them. Which group a file is in *is* its access rule.

### Route protection lives in one layout

`app/(protected)/layout.tsx` calls `requireSession()`, which redirects to
`/login?next=<where they were going>` when there is no session. One call covers
the whole subtree, so a page added under that group is protected because of
where its file is rather than because somebody remembered a check.

Nothing in `lib/session` or `proxy.ts` redirects — ADR 0007 is explicit about
why, and the short version is that a redirect issued from a fetch helper runs on
the sign-in page too. The screens decide.

**A layout is not told its own URL**, though, and the redirect needs one. So
`proxy.ts` stamps the requested path onto the request as `x-collabboard-path`
and `currentRequestPath()` reads it back. Two things keep that from being a
hole: the proxy `set`s rather than appends, on every path, so a client-supplied
copy is overwritten; and the value is only ever read through `safeReturnPath`,
which accepts a same-origin absolute path and turns everything else — `//evil`,
`/\evil`, `javascript:`, a control character — into `/app`. Sign-in with a
`next` parameter is the most valuable open redirect a site has, so it gets a
whitelist of shapes rather than a blacklist.

### Do not undo the API's work on enumeration

`apps/api` answers an unknown address and a wrong password with the same status,
the same message, and the same one argon2id derivation — issue #35 went to real
trouble for the last part. The screens keep it:

- **One message for 401**, naming neither field, written in `lib/auth/outcomes.ts`
  rather than relayed, so a server-side string changing cannot change the
  promise the sign-in screen makes.
- **The sign-up link under the form is unconditional.** A link that appeared only
  when the address was unknown is the same disclosure wearing a friendlier hat.
- **Sign-in validates presence only.** "Too short to be one of ours" is a
  statement about a stored value, and an account made under an older rule still
  has to be able to sign in.

Registration *does* disclose, with a 409, and that is the API's deliberate trade
(the alternative needs a mailer this service does not have). The screen relays it
plainly and offers sign-in.

Measured on this machine against a real API, 60 interleaved samples each through
`POST /api/auth/login`: wrong-password median 21.16 ms, unknown-address median
20.95 ms — a 0.21 ms difference against a 2.6 ms spread within either series.

### Rules mirror the API, and are not stricter

`lib/auth/rules.ts` holds every client-side check, and every one of them exists
in `apps/api/internal/auth`. Twelve characters minimum, 128 maximum, no
composition rules — because that is what the service enforces, and a client-side
rule the server would have accepted is a rejection with no authority behind it.

Two details worth keeping:

- **Lengths are counted the way Go counts them.** `len(password)` is bytes and
  `utf8.RuneCountInString` is code points; `String.length` is neither. A naive
  `.length >= 12` rejects a 24-byte emoji password the API is happy with.
- **The forms carry `noValidate`.** The browser's `type="email"` constraint is
  stricter than the API's "an `@` with something before it", so constraint
  validation is off and these rules decide. `type="email"` stays for the keyboard
  and the autofill.

The one check that is *not* in the Go service is "at least one character before
the `@`", which is in the database: `users.email` carries
`CHECK (position('@' IN email) > 1)`, so `@example.com` passes validation and
fails the insert as a 500. Filed as #76.

### What the UI does about the half-registered account (#34)

`Register` in `apps/api` commits the user and its password in one transaction and
the organization and membership in a second, so a failure between them leaves an
account that can authenticate and has nowhere to be. It is reachable, it is not
compensated, and the screens treat it as a real state rather than an
impossibility. It surfaces in two places:

**Signing in gets a 403.** The form does not show "email or password is
incorrect" — the credentials were right — and does not offer a retry. It says
the account exists, is not attached to a workspace, that signing up again will
not fix it, and to contact support. The last two sentences are the important
ones: the natural reaction to any other message is to re-register, which collects
a 409 on the address that already exists.

**Registration gets a 5xx**, and nothing in the response says whether an account
was created — it could be that, or a failure before the user row, or a lost
response. So the copy does not claim to know. It gives the instruction that is
correct in all three cases: try signing in, do not sign up again.

Both are reproducible. Delete a membership row and sign in as that user, which
is exactly the state a failure between the two transactions leaves:

```sql
delete from memberships m using users u
 where u.id = m.user_id and u.email = '<address>';
```

### Everything else worth knowing

- **The browser posts to `/api/auth/*`, never to the Go API.** `app/api/proxy`
  refuses the `auth` prefix, so there is no path from client JavaScript to
  `POST /auth/login` and no way to be handed a refresh token by asking.
- **`POST /auth/register` returns no tokens**, so the sign-up form posts twice:
  register, then login. When the second call fails — a 429 is the likely one,
  since both count against the same per-address budget — the screen says the
  account was created and points at sign-in, because telling the user to try
  again would send them into a 409.
- **`429` is respected.** `Retry-After` disables the submit button until it
  elapses. Every refused attempt still counts against the budget, so retrying
  early lengthens the block.
- **403 is ambiguous and is disambiguated.** This app's own CSRF guard also
  answers 403; it marks its refusals with `x-collabboard-refusal`, which
  `relayApiError` never sets. Without that the sign-in form would have to match
  on message text, which would make the copy load-bearing.
- **The shell reads the signed-in user's name from `GET /members`.** The session
  cookie has a user id and an organization, and `GET /me` adds a role and a
  session id — neither has a display name or an address. Listing every member to
  render one name is heavier than the job deserves; filed as #75. Every failure
  degrades to "Signed in" rather than redirecting, because `serverApi` cannot
  clear a cookie and a redirect from there would loop against `/login`, which
  bounces anyone who has one.

### Accessibility, as implemented

- A `<label for>` on every input, never a placeholder standing in for one, and an
  `autocomplete` token on each so password managers recognise the forms.
- A failed submit renders an error summary with `role="alert"`, moves focus to
  it, and lists each problem as a link to the input it is about.
- Fields carry `aria-invalid` and an `aria-describedby` covering both the hint
  and the error, so focusing one announces what is wrong with it.
- One focus ring, declared once in `globals.css` on `:focus-visible`, using an
  outline with an offset so it cannot reflow the page.
- Sign-out is a `<button>`, not a link: a GET that ends a session is one Next's
  own link prefetching would perform on the user's behalf.

### Files

```
app/(auth)/             /login and /register, plus the card they sit in
app/(protected)/        the signed-in shell; being in here is the access rule
components/auth/        the two forms, the labelled field, the error summary
components/app-shell.tsx  the signed-in frame (pure; takes resolved props)
lib/auth/rules.ts       validation, mirroring apps/api's numbers exactly
lib/auth/outcomes.ts    status → copy, including the enumeration-safe wording
lib/auth/routes.ts      the paths, and safeReturnPath's open-redirect check
lib/auth/submit.ts      posting a form to /api/auth/*
lib/session/require.ts  requireSession(): the one redirect in the app
lib/session/request-path.ts  how a layout learns which URL it is rendering
lib/session/viewer.ts   the signed-in user's name, via GET /members
```

### Session layer files

```
lib/api/errors.ts       ApiResult / ApiError, and the status → kind mapping
lib/api/types.ts        payload types + runtime parsers
lib/api/endpoints.ts    the endpoint catalogue, transport-agnostic
lib/api/http.ts         the one place a request is sent
lib/api/authenticated.ts  401 → single-flight refresh → one retry (no framework)
lib/api/server.ts       serverApi (read-only) and mutableServerApi
lib/api/browser.ts      the Client Component client, via /api/proxy
lib/session/cookies.ts  the three cookies and their attributes
lib/session/refresh.ts  single-flight + the post-rotation grace window
lib/session/origin.ts   the same-origin (CSRF) check
proxy.ts                refreshes before the render
app/api/auth/*          login, register, refresh, logout, session, organization
app/api/proxy/*         authenticated pass-through for Client Components
```

## Layout

```
app/                 routes (App Router). page.tsx is the public landing page,
                     (auth)/ and (protected)/ are the two access rules,
                     healthz/route.ts is the readiness signal, api/ holds the
                     session Route Handlers and the authenticated proxy.
components/          presentational components, kept pure so they are unit testable
lib/                 the API client and session layer, the auth screens' rules
                     and copy, the readiness policy, the structured logger
proxy.ts             pre-render session refresh + the requested-path stamp
                     (Next 16's renamed middleware)
instrumentation.ts   server startup hook: validates + logs the resolved API_URL
Dockerfile           multi-stage build of the deployable image (context: repo root)
__tests__/           vitest + React Testing Library
```

The data fetch lives in the Server Component and the rendering lives in a pure
component that takes the resolved result as a prop — that split is what makes
the healthy, degraded, and unreachable branches testable without a network.
