# apps/web

Next.js (App Router, React Server Components) front end for CollabBoard.

What exists: a public landing page that server-side fetches `GET /healthz` from
the Go API, the [authentication screens](#the-authentication-screens) (register,
sign in, sign out, route protection),
[the signed-in workspace](#the-workspace-projects-boards-and-people) — projects,
boards and the people in an organization — and
[the board view](#the-board-view-columns-and-cards), which renders a board's
columns and cards and lets you edit them: create, rename, reorder and delete
columns, create, edit and delete cards, and
[move a card](#moving-a-card-the-headline-interaction) by dragging it or with
the keyboard — and, since #66, [live updates](#live-updates-66): a card moved or
edited by somebody else appears on every open board in about ten milliseconds.

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
| `/app` | The signed-in workspace | Required |

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
the account exists, is not attached to a workspace, and that signing up again
will not fix it. That last part is the important one: the natural reaction to any
other message is to re-register, which collects a 409 on the address that already
exists.

It used to end there, with "contact support", because it had to — nothing created
an organization for an existing account. `POST /api/v1/organizations` (#34, [ADR
0009](../../docs/adr/0009-tenantless-account-recovery.md)) removed that dead end,
so the 403 now renders `components/auth/workspace-recovery.tsx` instead of a
sentence: an optional workspace name, a button, and a sign-in on the way out.

The endpoint takes an email and a password rather than a token, because an
account with zero memberships **cannot hold one** — login refuses to issue it,
the issuer refuses a nil tenant, the verifier refuses a zero `org` claim. The
user typed both into the form a moment ago, so the affordance uses what is
already there rather than asking twice. The password therefore lives across the
transition, exactly where a controlled input already kept it: in the sign-in
form's `useState`, never copied into the recovery component (which receives a
*getter*, not a value), and never in `localStorage`, a cookie, a URL or a log
line. `workspace-recovery.tsx`'s module comment argues that decision in full,
including why re-prompting was rejected.

Two answers on that route are not failures and are not drawn as ones. A **409**
means the workspace already exists — another tab, or two clicks that raced — so
the screen switches to a notice pointing at the sign-in form. A **429** is the
likely one, because the route is charged against the *sign-in* budget and charged
before the credential is checked; it reports the wait rather than a generic
error, and stops both buttons until it elapses.

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
components/auth/        the two forms, the workspace-recovery affordance,
                        the labelled field, the error summary
components/app-shell.tsx  the signed-in frame (pure; takes resolved props)
lib/auth/rules.ts       validation, mirroring apps/api's numbers exactly
lib/auth/outcomes.ts    status → copy, including the enumeration-safe wording
lib/auth/routes.ts      the paths, and safeReturnPath's open-redirect check
lib/auth/submit.ts      posting a form to /api/auth/*
lib/session/require.ts  requireSession(): the one redirect in the app
lib/session/request-path.ts  how a layout learns which URL it is rendering
lib/session/viewer.ts   the signed-in user's name, via GET /members
```

## The workspace: projects, boards and people

Four routes under `/app`, all behind `app/(protected)/layout.tsx`:

| Route | What it is |
| --- | --- |
| `/app` | The organization's projects, and the form that creates one |
| `/app/projects/<id>` | One project: its boards, rename, archive |
| `/app/projects/<id>/boards/<id>` | One board: its columns and cards — see [the board view](#the-board-view-columns-and-cards) |
| `/app/members` | Everyone in the organization, and the add-member form |

### State lives in the URL, so a reload and a paste both work

The project you are looking at and the board you are looking at are path
segments, not a selection held in a client component. A reload re-renders the
same screen on the server; a link opens the same screen for a colleague who has
access; the back button means what it looks like it means.

The ids are therefore attacker-supplied, which is fine and is why nothing
validates them locally: an id that names nothing and an id belonging to another
organization are both resolved inside the caller's own tenant and both come back
404 (`crud.go`'s `notFound`). The screens render one state for both, because they
genuinely cannot tell them apart and copy that implied otherwise would hand back
the bit the API withholds.

**A board's URL is nested under its project even though the API's is not.**
`apps/api` addresses a board flatly at `/boards/:id` and says why — a nested API
path invites a handler to trust the ancestors in it. A URL has the opposite
problem: it has to render a breadcrumb, and fetching the board to discover its
project would be a round trip for one line of text. So the project id is in the
path and the board page **checks** it: `board.projectId` must equal the segment
it was reached through, or the page renders not-found. A mismatched pair would
otherwise show a real board under a breadcrumb naming a project it is not in.

### Reading is server-side; writing is a Client Component through the proxy

Every list is a Server Component using `serverApi` — the session is already
there, there is no round trip, and no fetching code reaches the browser. Every
form is a Client Component using `browserApi()` through `/api/proxy`, because a
form needs pending state, per-field errors and focus management. After a
successful write the form calls `router.refresh()`, so the list re-renders from
the server rather than being patched in client state: one source of truth, and
the row that appears is the row the API stored.

Two consequences worth knowing:

- **Nothing is optimistic.** The API trims names and can reject a write, so the
  screen shows what was stored rather than what was typed.
- **`loading.tsx` per segment is the only loading state**, because there is no
  client-side fetch on first paint to cover. It is Next's Suspense fallback for
  the segment and it is served in the first flush.

### Empty states

The first screen a new account sees is `/app` with no projects, so it is a screen
rather than an absence: a heading, three numbered steps explaining the
project → board → card vocabulary this app never otherwise defines, and the
create form with focus already in it. A project with no boards and a workspace
with one person get the same treatment for the same reason.

### Archiving is one-way, and the UI says so instead of hiding it

`POST /projects/:id/archive` cannot be undone — there is no unarchive and no way
to list archived projects (#49). Issue #62 allowed either hiding the control or
making the consequence explicit; this takes the second, because hiding it does
not make the door reversible, it makes the capability unreachable while leaving
the endpoint exactly as final.

So `components/projects/archive-project.tsx` sits in a collapsed disclosure and,
when opened, states three things before the confirmation:

- it **cannot be undone**, and the project leaves every list in the product;
- **nothing is deleted** — `ArchiveProject` sets `archived_at` and the boards and
  cards stay in the database;
- **this page's address keeps working**, because `GetProject` has no
  `archived_at` predicate. Only `ListProjects` filters. So a saved link is the
  only route back to those boards, and that is worth knowing *before* confirming
  rather than discovering after.

Confirming requires typing the project's name. A confirm dialog is a reflex;
typing is a decision, and for an action with no undo the few seconds are cheap.

Arriving at an archived project by URL renders an explicit archived panel and
withdraws the rename and archive controls — renaming something nobody can find
is not a useful offer.

### The caller's role comes from `GET /members`, not from the token

`session.organization.role` is a claim minted at login and re-derived at most
once per access-token lifetime, so a promoted or demoted account carries a stale
one. `apps/api` refuses to trust it for exactly this reason: ADR 0008 has
`AddMember` read the caller's row from `memberships` inside the tenant
transaction, and again in the transaction that inserts.

`GET /members` lists that same table for that same tenant, and `/app/members`
already has it. So `lib/workspace/roles.ts` derives the role from the list —
one request, and the answer the server will give. An `owner` or `admin` is
offered the form; a `member` is shown a sentence saying who can add people, not a
button that would 403. A caller who is **not in the list at all** — a revoked
membership, or #34's half-registered account — is offered nothing and told to
sign out and back in.

The role choice is bounded the same way: an owner may grant `member` or `admin`,
an admin may grant `member` only, and nobody is offered `owner`, because
`validateAddMember` refuses it as a property of the endpoint.

### Adding a member is a direct add, and the copy does not pretend otherwise

ADR 0008: there is no invitation and no mailer, so the person is in the workspace
immediately and **is not notified**. The success message says so. An address with
no account is a 404 and always will be — this path never creates a user — so the
copy says "they need to sign up first" rather than "check the address", which
would send the user round a loop they cannot exit.

### Files

```
app/(protected)/app/                      the four routes, plus a loading.tsx each
components/workspace/workspace.module.css one stylesheet for the whole signed-in app
components/workspace/fields.tsx           labelled input/textarea/select (ids are props)
components/workspace/states.tsx           empty, error and loading states
components/workspace/page-header.tsx      title, lede and breadcrumbs
components/projects/                      list, create, rename, archive
components/boards/                        a project's boards, and one board itself
components/members/                       list, add
lib/board/snapshot.ts                     columns × cards, in the API's order
lib/board/mutations.ts                    what one edit does to that shape
lib/workspace/routes.ts                   every URL, and why the board one nests
lib/workspace/rules.ts                    validation mirroring crud.go's limits
lib/workspace/roles.ts                    who may add, and where the role is read from
lib/workspace/outcomes.ts                 ApiError → copy, for loads and for writes
lib/workspace/format.ts                   timestamps, in a fixed locale and zone
```

### Known rough edge

`app/(protected)/layout.tsx` awaits `loadViewer()`, which is a `GET /members`
call made only to render one name (#75, #78). Because that await is *above* every
`loading.tsx`, the segment fallbacks cannot paint until it resolves — on a slow
API the whole signed-in area waits on a request none of the pages need. #78
deletes that call, which removes the problem rather than working around it.

It is also why a board page makes **five** API requests and not four: four of
its own, plus that one. The five are constant — a board with 240 cards makes the
same five as an empty one.

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
app/api/auth/*          login, register, refresh, logout, session, organization,
                        first-organization (the tenantless recovery path)
app/api/proxy/*         authenticated pass-through for Client Components
```

## The board view: columns and cards

The core screen. `/app/projects/<id>/boards/<id>` renders a board as columns of
cards and lets you change it, including
[moving a card](#moving-a-card-the-headline-interaction) between and within
columns. Live updates are #9, and the note under the board says so in words
rather than offering a disabled control that cannot do it.

### The order is the API's, and the client never computes one

This is the rule the whole screen is built around, and
[ADR 0004](../../docs/adr/0004-card-ordering.md) is why. A card's rank is a
server-allocated `numeric` that appears in **no response** and is accepted by
**no endpoint**; `apps/api` returns cards already ordered
(`ORDER BY column_id, position, id`) and that sequence *is* the answer.

So `lib/board/snapshot.ts` groups the flat cards list into columns in a single
pass that preserves input order, and there is no comparison anywhere in it.
There is nothing to sort by even if you wanted to: `created_at` is
second-precision and stops matching the board the moment anyone drags anything,
and `title` is not an order at all.

If a change here ever reaches for `.sort()`, the bug is upstream — either the
API's `ORDER BY` is wrong or the request was not the one the screen needed.

The reason this matters beyond correctness is the card move. Drag-and-drop posts
`{"column_id": …, "after_card_id": …}` — an *anchor*, a claim about one row —
and an anchor is only meaningful against the list the server actually has. A
client carrying its own ordering model would be computing anchors against a list
nobody else agrees with, and a stale anchor is a 409 rather than a wrong-looking
success.

### Three requests for the whole board, never one per card

`GET /boards/:id`, `GET /boards/:id/columns`, `GET /boards/:id/cards`, plus
`GET /projects/:id` for the breadcrumb. All four in one `Promise.all`, because
none depends on another's answer — the tempting "read the board, then read its
contents only if it exists" is a waterfall that doubles time to first paint on
every successful load to save two requests on the rare failing one.

`GET /boards/:id/cards` returns the entire board, so the request count does not
move with the number of cards. Measured against the production build and a real
API: a 240-card board renders in a median 15 ms (TTFB 10 ms) and makes the same
five API requests as a five-card one. 284 kB of HTML, 25 kB over the wire
compressed.

Only `GET /boards/:id` distinguishes a board that does not exist from one that
does. The two list endpoints answer `200 []` for an unknown board id, because
they filter by `board_id` inside row-level security rather than resolving the
board first — which is what makes an empty cards list mean "empty board" and
makes 404 the board request's business alone. Firing them for a board the caller
cannot see leaks nothing for the same reason.

### The open card is a search parameter

`?card=<id>`, not a route of its own. A card is a detail of the board and the
board stays on screen behind it; a `/cards/<id>` route would either lose that
context or need intercepting routes to fake keeping it, and the fake comes apart
on exactly the reload or pasted link the URL exists to survive.

It also costs no request. The board already fetched every card, so opening one
is a lookup in memory — `GET /cards/:id` would be a round trip for bytes the
page is holding, and it would put the detail panel one network failure away from
a board that loaded fine. An id that names nothing on this board renders a "card
not found" panel with the board still behind it, without asking the API whose
card it was.

### Where the Server/Client line falls — #66 inherits this

**The page is the only thing that touches the API or the session.** It fetches,
checks the two 404 cases, groups the responses, and hands plain serialisable
props to one client boundary:

```
page.tsx                     Server Component: four requests, the 404 checks,
                             and the grouping. The only async thing here.
lib/board/snapshot.ts        pure: columns × cards → the board's shape
lib/board/mutations.ts       pure: snapshot + one edit → the next snapshot
components/boards/board-view.tsx      "use client": the optimistic store, the
                                      columns, and the detail panel
components/boards/board-mutation.ts   apply → send → refresh, for one edit
components/boards/board-controls.tsx  the composers and the column tools
components/boards/card-drag.tsx       the pointer gesture, and one card's grip
components/boards/card-moves.ts       moves, queued per card, and the 409
components/boards/card-detail.tsx     one card, read or editable
components/boards/board-skeleton.tsx  the loading.tsx fallback
```

#63 predicted that making the board interactive would be `"use client"` at the
top of `board-view.tsx` with `page.tsx` unchanged, and #64 confirmed it: the
props cross the RSC boundary unaltered because `Card` and `Column` are strings
all the way down. Two things did change in the page, both consequences of a card
now being deletable — it passes the **raw** `?card=` value down rather than a
resolved one, and `BoardView` owns the detail panel so that one optimistic store
covers both a card's tile and the panel that edits it.

The three rules that came with the boundary, all still in force:

1. **`page.tsx` is not a Client Component.** The session and the token live on
   the server; a client page would refetch the board through `/api/proxy` on
   every mount and give up first paint for nothing.
2. **No board component fetches.** One request per column is the failure mode
   this design exists to avoid, and it starts with one innocent `useEffect`.
   Writes go out through `/api/proxy` and are followed by `router.refresh()`,
   so the *read* still happens once, on the server.
3. **Reorder by asking the server, not by sorting an array.** A column or card
   move posts to `/move` with a neighbour's id and then re-reads. The splice in
   `lib/board/mutations.ts` is a display held for the duration of the request;
   it never becomes the source of truth, or the client would have invented the
   rank ADR 0004 refused to publish.

Selection is a URL rather than state, so there is nothing to lift into a
provider either.

### Moving a card: the headline interaction

Drag a card within a column or into another one, or move it with the keyboard.
Both end in one `POST /cards/:id/move` carrying `{"column_id": …,
"after_card_id": …}` — an **anchor**, never an index and never a rank, because
[ADR 0004](../../docs/adr/0004-card-ordering.md) keeps the rank off the wire and
an index is a claim about a list this client may already be wrong about.

**The gesture decides nothing.** A drag reports two facts — what the pointer is
over, and which side of it — and the keyboard reports an arrow key. Both are
turned into the same `CardMove` by pure functions in `lib/board/mutations.ts`
(`cardDropTarget`, `cardNudge`), which is why the ordering logic is unit tested
as arithmetic and why the two input methods cannot drift apart.

**The keyboard reaches every placement a drag does.** Enter on a card's grip
lifts it; the arrow keys move it up and down its column and into the columns
beside it; Enter drops it and Escape leaves it alone. Each keypress moves a
*proposal*, drawn through the same reducer as an optimistic edit, so crossing
half the board still costs one request — the same as one drag. Every change of
position is announced through a live region, because a reorder fires no
accessibility event of its own and a silent one is invisible even when it works.

**Two rapid moves of one card are serialised.** Left alone they race: the server
resolves two moves of the same row as last-writer-wins, and "last" means last to
*arrive*. So each card has a queue in `components/boards/card-moves.ts` — a
move's request waits for that card's previous request to be answered, while its
optimistic change is already on screen, so the queue costs nothing to look at.
Different cards never wait for each other.

**A stale anchor is a 409, and the card goes back.** If someone else moved the
card you dropped this one next to, the anchor names nothing in that column and
the API refuses the move. The optimistic move comes off the board, the board
re-reads, and a message says the card is back where it started. It deliberately
**does not retry**: retrying means choosing a new anchor from the refreshed
board, and "the third slot" is an index — the exact claim ADR 0004 refused,
because the server cannot tell a deliberate placement from a stale one.

#### The library, and what it cost

`@dnd-kit/core` + `/sortable` + `/utilities`, measured rather than estimated:
**+48.8 kB raw, +16.0 kB gzipped** of client JavaScript (the whole app goes
224.4 kB → 240.4 kB gzipped), loaded only on the board route.

It is there for the parts a hand-rolled implementation does not get. HTML5
drag-and-drop is free and would have covered a mouse, but mobile browsers fire
none of its events, so touch support would simply not exist; `TouchSensor` is
one line. It also handles auto-scrolling the container under the pointer, which
this screen needs in both axes — a horizontal scroller of columns that are each
their own vertical scroller. And it is the half that **cannot be unit tested**:
jsdom has no layout, so no pointer gesture, library or otherwise, can be driven
in `vitest`.

Rejected: `react-beautiful-dnd` (deprecated by Atlassian); `@hello-pangea/dnd`,
its maintained fork, which is 31 kB gzipped and reports drops as source and
destination *indices*, the one currency this API refuses; and `@dnd-kit/react`
0.5.0, the actively developed successor, which is pre-1.0.

Two risks, named rather than discovered later: `@dnd-kit/core` 6.3.1 was
published in December 2024 with nothing since, and it calls `react-dom`'s
`unstable_batchedUpdates`, which React 19.2.8 still exports — checked, not
assumed.

dnd-kit's `KeyboardSensor` is **not** used. It moves a card by pixels and re-runs
collision detection, which on independently scrolling columns is unpredictable
and, again, undrivable in jsdom. The keyboard path walks the list instead.

#### What a move costs

Against the production build and a real API: `POST /cards/:id/move` is a median
13 ms. The `router.refresh()` that follows it — the whole-board re-read #64
accepted — is a median 15–21 ms, and it is **coalesced**: six moves fired as
fast as the keyboard allows produced four re-reads, not six, and the board still
converged to the server's answer without a reload. So the burstiness a drag adds
over a rename turns out not to multiply the re-read, and rule 3 stands without a
workaround.

### Editing: optimistic, and rolled back by construction

Every edit applies on screen immediately, goes out through `/api/proxy`, and
then either is confirmed by a fresh server render or disappears.

The mechanism is React's
[`useOptimistic`](https://react.dev/reference/react/useOptimistic), whose store
lives in `BoardView` and whose reducer is the pure function in
`lib/board/mutations.ts`. A control calls the setter from inside the transition
that sends the request, and React holds such a value **only while that
transition is pending**:

- **Success** — `router.refresh()` runs inside the transition, so it stays
  pending until the Server Component has re-rendered. The optimistic value is
  dropped at the moment the real one replaces it, so the board does not flash
  through its old state on the way to its new one.
- **Failure** — the transition ends, the prop never changed, and the board is
  exactly as it was.

**So there is no `undo` function anywhere, and that is the point.** The
alternative — `useState` plus a hand-written inverse per operation — puts the
correctness of the failure path in code that only runs when the server refuses,
which is the path nobody exercises locally. `__tests__/board-editing.test.tsx`
tests it anyway, for all seven operations, and each of those tests asserts the
change is on screen *before* the refusal as well as gone after it, so it fails
against a board that is not optimistic as loudly as against one that never
reverts.

Two consequences worth knowing:

- **The optimistic store sits above everything that can be deleted.** A control
  that deletes a column lives inside that column and unmounts the instant the
  delete applies, so neither the optimistic value nor the failure message can be
  held there. The failure banner is on the board for the same reason.
- **A row the server has not acknowledged is not addressable.** Its id was
  invented by this client (`pending:` prefix), so its tile is rendered as inert
  text rather than a link to `?card=<invented id>`, and a column in that state is
  shown without its Edit control.

### The limits are the API's, counted the way Go counts

200 code points on a column name and a card title, 10,000 on a description —
`maxNameLength` and `maxDescriptionLength` in `apps/api/internal/api/crud.go`.
Checked before the request rather than after the 400, and never stricter than
the service: exactly 200 is accepted here because `requiredText` accepts it.

Counted with `codePointLength`, because the API counts with
`utf8.RuneCountInString` and `String.length` is neither bytes nor code points.
The `maxLength` attribute is a courtesy stop and goes through `maxLengthFor`,
which doubles the limit — `maxLength` counts UTF-16 code units, so passing 200
would stop the browser at 100 emoji and refuse input the API would have taken.

Input is trimmed before it is validated and sent, because the API trims before
it stores. An empty description clears the field (`allowEmpty: true`); an empty
title does not (`allowEmpty: false`). A form that changed nothing closes instead
of submitting, because `PATCH` answers 400 to a body that mentions no field.

**A card edit sends only the fields that changed.** `PATCH /cards/:id` leaves
out what it is not given, so resending an untouched description would overwrite
a colleague's edit to it with the copy this page loaded a minute ago.

### Deleting a column says how many cards go with it

`DELETE /columns/:id` cascades to the column's cards, and there is no undo and
no way to list deleted rows (#49). So the confirmation states the number:
*"Delete Doing and its 12 cards?"* — because "Are you sure?" is a question
nobody reads, and the count is the fact that changes the answer.

It is a panel rather than `window.confirm`, which cannot be styled or tested,
blocks the event loop, and is suppressible browser-wide. Deleting a card is
confirmed the same way, and on success the panel navigates back to the board
without `?card=` rather than leaving the URL pointing at a card that is gone.

Unlike `archive-project.tsx`, neither asks you to type a name. That is a
judgement about proportion — a project is the whole tree and its confirmation
guards a workspace-wide disappearance, where a column is one list a person can
see the whole of, with its cost stated in the sentence.

### States, all of them reachable

| State | What renders |
| --- | --- |
| Loading | `loading.tsx` — column-shaped, in the first flush |
| Empty board | "This board has no columns yet", what a column is for, and the form that adds the first one |
| A write the server refused | The change comes off the board, and a focused `role="alert"` says why |
| A row not yet acknowledged | Drawn dimmed and inert — no link, no Edit — because its id is invented |
| Empty column | "No cards in this column." in place of the list |
| Board not found / another tenant's | One 404 sentence covering both, per `crud.go`'s `notFound` |
| Board in a different project | The same 404 — `board.projectId` is checked against the URL |
| Columns or cards failed | The header and breadcrumb stay; the failure is about the contents |
| API unreachable | "The server did not answer", with a retry — one 10 s timeout, not four, because the requests are parallel |
| `?card=` names nothing here | A "card not found" panel, board still behind it |

### One layout note

`components/boards/board.module.css` is the one screen that does not use
`workspace.module.css`. The rest of the signed-in app is a document — a single
column inside a 60rem measure — and the board is a horizontally scrolling region
of fixed-width columns, each with its own vertical scroller so that 240 cards
scroll inside a column rather than making the page 240 rows tall. Sharing
`.card` between a link tile in a responsive grid and a card in a dense stack
would mean bending the class every other screen already uses.

Two accessibility details that are load-bearing rather than decorative: each
column's card list is an `<ol>` because the order is the board's meaning, and it
carries `tabindex="0"` because a scroll container with no focusable child is
unreachable without a pointer — which is every column whose cards run past the
fold.

## Live updates (#66)

A card moved or edited by somebody else on the same board appears here without a
reload. Measured end to end with two Chromium contexts, two accounts, one board:
**p50 9 ms, p95 12 ms, max 14 ms over 30 moves**, from the moment one browser
issues the move to the moment the other browser's DOM shows it — against the
project's ~200 ms target. The server's own fan-out is p99 1.64 ms; the rest is
the mover's HTTP round trip, the relay, and React rendering.

### The browser has no token, so it does not open the WebSocket

The API authenticates a WebSocket handshake with a bearer token in a
`Sec-WebSocket-Protocol` offer, a mechanism that exists *because* browsers
cannot set handshake headers. [ADR 0007](../../docs/adr/0007-web-session-storage.md)
says the browser never holds a bearer token. Both are load-bearing, so they were
resolved rather than traded: **the handshake happens on the Next server, where
the token already is, and the browser gets a same-origin event stream.**

```
browser ──SSE──▶ GET /api/realtime/boards/:id   (cookies; no credential in JS)
                 app/api/realtime/boards/[boardId]/route.ts
                        │
                        └──WebSocket──▶ GET /api/v1/ws
                           Sec-WebSocket-Protocol:
                             collabboard.v1, bearer.collabboard.v1.<jwt>
```

[ADR 0010](../../docs/adr/0010-realtime-browser-credential.md) records the
options, including the one this refuses — handing the page a fifteen-minute
full-scope token, which an XSS could exfiltrate and replay from anywhere — and
what the relay costs: two connections per viewer, and the web tier on the
realtime path.

### Three layers, and the order they are applied in

```
snapshot (prop)      what the Server Component last read     the truth
  + live log         everyone else's events since that read  seconds old
    + useOptimistic  this user's own unconfirmed edits       milliseconds old
```

Each layer is younger and less certain than the one below it. **That ordering is
the whole answer to the flicker question:** an inbound `card.moved` for a card
you are dragging lands *underneath* your own optimistic move, which is replayed
on top, so the card does not jump out from under the pointer. When your move
settles, `router.refresh()` brings the server's answer and last-writer-wins
decides — the semantic ADR 0004 and ADR 0005 already chose.

The one event that is not applied immediately is a **create**, and only while
this client has an unconfirmed create of its own. A pending card carries a
`pending:` id that `lib/board/mutations.ts` made deliberately unmatchable to a
server id, so there is no way to tell "my card coming back" from "somebody
else's new card", and drawing both is a visible duplicate. While anything is
pending, creates are left to the re-read, which replaces the placeholder and adds
the real row in one step.

### Every subscribe re-reads the board, and that is the recovery design

[ADR 0005](../../docs/adr/0005-realtime-event-delivery.md) is explicit that Redis
pub/sub is at-most-once and holds nothing: anything published while a client is
between connections is gone, and nothing will resend it. So every `subscribed`
frame — the first one as much as the ones after a reconnect — triggers a full
re-read.

Demonstrated rather than asserted: with one browser's stream refused at the
network layer, the other moved a card; the disconnected browser was checked to be
**wrong**, then reconnected and was correct. Only the re-fetch could have told
it, because the event no longer existed anywhere.

The live log is also *replayed* over each new snapshot rather than discarded,
which is why every function in `lib/realtime/apply.ts` is idempotent. An event is
retired once a read that *started after it arrived* has landed — sound because
ADR 0005 publishes after the commit and before the response, so an event observed
at time *t* proves its write was durable before *t*.

### The close codes mean different things

| Code | Means | This client |
| --- | --- | --- |
| `4001` | access token expired | refresh, **then** reconnect. Not a sign-out — a 15-minute credential ending on schedule is the normal life of an open tab |
| `4002` | dropped as a slow consumer | back off, reconnect, re-read. Events were missed *by definition*, which is the case ADR 0005 built the re-fetch for |
| `4003` | membership revoked | stop. Every retry would be authorised, refused and closed again |
| `1001` | instance restarting, or no pong | reconnect, honouring the server's `reconnect_after_ms` hint so a rolling deploy is not a thundering herd |
| `1003`/`1009` | the server rejected what we sent | stop. A bug here, not a condition |

Each was provoked for real, not reasoned about: `4001` by running the API with a
40-second token TTL and watching the browser refresh and reconnect while staying
signed in; `4002` by a deliberately stalled raw consumer against a one-frame send
buffer; `4003` by revoking a membership and watching the 30-second re-authorisation
sweep close the socket.

Reconnects back off exponentially with full jitter, from 500 ms to 30 s, **with a
100 ms floor**. The floor is not a rounding detail: full jitter returns
single-digit milliseconds, and a retry that comes back in 3 ms against an
endpoint failing instantly is a self-inflicted denial of service. It was observed
before it was fixed — the loop exhausted a Vitest worker.

### The connection state is on screen

`components/boards/connection-status.tsx`, above the columns: *Live*,
*Connecting…*, *Reconnecting…*, or *Not live* with a sentence saying why. The
failure mode of a realtime feature is **silence**, and silence is
indistinguishable from nobody else editing — which is
[#53](https://github.com/AndyV99/collabboard/issues/53)'s complaint. Note the
split: this reports *this client's connection*, which it knows first-hand. It
does not report whether the server's Redis fan-out is healthy, because the server
does not say. Making it say so is still #53.

### Files

```
app/api/realtime/boards/[boardId]/route.ts  the SSE route: session in, stream out
lib/realtime/relay.ts        server side: one WebSocket in, one event stream out
lib/realtime/stream.ts       the contract between the two halves, and the ws:// URL
lib/realtime/protocol.ts     the wire format: frames and events, parsed into a union
lib/realtime/apply.ts        an event -> a board change, idempotently
lib/realtime/recovery.ts     what each close code means, and how long to wait
lib/realtime/client.ts       the browser's connection and reconnect state machine
components/boards/use-board-live.ts       the live log, the re-read, and when to retire
components/boards/connection-status.tsx   what the user is told
```

Every dependency that would make the client untestable — `fetch`, the clock, the
timer, the jitter, the token refresh, the socket — is an argument. There is no
`setTimeout` and no `Math.random` in the body of `lib/realtime/client.ts`, and
the tests drive the schedule by hand rather than waiting for it. That is the rule
`__tests__/board-editing.test.tsx`'s header lays down, applied to a layer that
made it much easier to break.

## Layout

```
app/                 routes (App Router). page.tsx is the public landing page,
                     (auth)/ and (protected)/ are the two access rules,
                     (protected)/app/ is the workspace, healthz/route.ts is the
                     readiness signal, api/ holds the session Route Handlers,
                     the authenticated proxy, and the realtime event stream.
components/          presentational components, kept pure so they are unit testable
lib/                 the API client and session layer, the auth screens' rules
                     and copy, the workspace's rules/roles/copy, the realtime
                     protocol and reconnect policy, the readiness policy, the
                     structured logger
proxy.ts             pre-render session refresh + the requested-path stamp
                     (Next 16's renamed middleware)
instrumentation.ts   server startup hook: validates + logs the resolved API_URL
Dockerfile           multi-stage build of the deployable image (context: repo root)
__tests__/           vitest + React Testing Library
```

The data fetch lives in the Server Component and the rendering lives in a pure
component that takes the resolved result as a prop — that split is what makes
the healthy, degraded, and unreachable branches testable without a network.
