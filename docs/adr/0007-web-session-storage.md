# 0007. Web session storage: httpOnly cookies set by a Next Route Handler

Date: 2026-08-09
Status: accepted

## Context

`POST /api/v1/auth/login` returns a 15-minute JWT access token and a 14-day
opaque refresh token **in the response body**. `apps/api/internal/api/auth.go`
says why, in the doc comment on `sessionResponse`:

> The refresh token is in the body rather than a Set-Cookie because the client
> is a separate origin ... Flagged in the PR as an invented decision: it is the
> right call for a token-based SPA and the wrong one if the web app ever becomes
> same-origin server-rendered.

The web app is same-origin server-rendered. It is Next.js App Router with React
Server Components, deployed as one image behind one origin, and the review of
PR #35 flagged exactly this. Issue #59 exists to settle where the tokens live
before any screen depends on the answer, because every later web issue inherits
it and a session model is expensive to unpick.

Three facts from `apps/api` constrain the answer, and all three are load-bearing:

1. **Refresh tokens rotate.** `SessionStore.Rotate` mints a successor on every
   refresh.
2. **Reuse revokes the session.** The superseded record is deliberately kept so
   that presenting it again is detectable, and `Service.Refresh` responds to
   `ErrRefreshReused` by killing the session outright.
3. **Refresh re-checks membership.** It is not a pure rotation; it is also how a
   removed member stops being one within one access-token lifetime.

(1) and (2) together mean a refresh stampede is not a wasted round trip. It is a
self-inflicted logout: one rotation, N−1 replays, session gone.

## Options

**Client-held tokens in memory.** The shape the API was built for: the browser
receives both tokens and holds them in a module variable. Rejected on three
counts. Tokens in a JS variable are unreachable during server rendering, so every
authenticated screen becomes a Client Component with a loading state — which
throws away the reason this app is RSC-first, and the reason issue #16 went to
the trouble of keeping `API_URL` server-only. They are lost on reload, so a page
refresh is a re-login unless the refresh token is persisted somewhere the browser
*can* read, which is the thing being avoided. And a refresh token readable by
script is exfiltrable by any XSS in this origin — a 14-day credential, where the
access token is a 15-minute one.

**Tokens in `localStorage` or a non-httpOnly cookie.** Fixes reload, keeps every
other problem, and makes the XSS exposure permanent rather than tab-scoped. Not
seriously considered; recorded because it is the default people reach for.

**httpOnly cookies set by the Go service.** The tidiest shape on paper: the API
sets `Set-Cookie` itself. Rejected here because it is an API change, and this was
a web issue — `apps/api` would need cookie handling, a CSRF defence it does not
have, and a decision about whether the token-in-body shape stays for other
clients. It is a legitimate future direction and is not foreclosed by this
record.

**httpOnly cookies set by a Next Route Handler.** The web app calls the API,
takes the tokens out of the body, and writes them into its own httpOnly cookies
on its own origin. Chosen.

## Decision

**The browser never receives a token. A Route Handler on the web origin
exchanges credentials with the Go API and stores the result in httpOnly
cookies.**

No change to `apps/api`. The service keeps returning tokens in the body; this app
is simply the only reader of that body.

**Three cookies, all `httpOnly`, `SameSite=Lax`, `Path=/`, `Secure` outside
development.** `cb_at` (access token), `cb_rt` (refresh token), and `cb_session`
— user id, active organization, and access-token expiry. The metadata cookie is
not a credential and is `httpOnly` anyway, so the rule is "no session cookie is
readable by script" rather than a rule with an exception. It is what lets the
refresh happen *before* a request fails, and it saves a `GET /me` per render.

**Server Components read; only Route Handlers, Server Actions and `proxy.ts`
write.** This falls out of the framework — `cookies().set()` is illegal during
rendering — but it is a rule with teeth here rather than a nuisance. A Server
Component that refreshed would rotate the refresh token and have nowhere to put
the successor, spending the session's credential for the sake of one render. So
`lib/api/server.ts` exposes two clients: `serverApi`, which cannot refresh, and
`mutableServerApi`, which can.

**`proxy.ts` refreshes before the render.** Next 16's renamed middleware runs
before a page renders and can set cookies, so it is where an expired access
token is renewed. By the time a Server Component runs, the token is fresh or
absent — never expired-but-renewable. It is excluded from `/api/*`, because those
handlers refresh for themselves and two refreshers on one request would spend the
same token twice.

**Client Components reach the API through `/api/proxy/*`**, a Route Handler that
attaches the bearer token server-side. It carries an allowlist of resource roots;
`auth` is not on it, so there is no path from client JavaScript to
`POST /auth/login` or `/auth/refresh` and therefore no way for a browser to be
handed a refresh token by asking.

**One refresh at a time, and a grace window on the token that was spent.**
Concurrent callers holding the same refresh token share one request. That alone
turned out not to be enough: the end-to-end run for #59 produced one refresh
*and* one reuse detection, because requests already in flight when the rotation
happened were still carrying the pre-rotation cookie, found an empty map, and
replayed it. A settled outcome is therefore remembered against the spent token
for ten seconds. This is the grace window a rotating-token implementation
conventionally puts on the server, implemented on the client side so that no API
change is needed.

**CSRF is defended twice.** `SameSite=Lax` stops the browser attaching session
cookies to a cross-site POST; `lib/session/origin.ts` additionally requires
`Sec-Fetch-Site: same-origin`/`none`, or a matching `Origin`, on every
state-changing route. One control is how CSRF happens.

## Consequences

**An XSS in this origin can still act as the user; it cannot become the user
elsewhere or later.** That is the honest boundary of this decision. Script in the
page can call `/api/proxy/*` and the cookies ride along. What it cannot do is
read a credential, so it cannot exfiltrate a 14-day session to an attacker's
machine, and it loses everything the moment the page is closed. That is a large
reduction in blast radius, not immunity, and it is why the allowlist on the proxy
matters as much as the cookie flag.

**Server code can read the refresh token.** `cookies()` in a Server Component
returns every cookie, `cb_rt` included. Scoping the cookie's `Path` to
`/api/auth` would fix that, and would also stop `proxy.ts` — which runs on page
paths — from ever seeing it, which is where the renewal has to happen. The
narrowing that was possible instead: `ServerSession` has no refresh-token field,
`getRefreshToken()` is a separate function, and "who can get one" is a grep.

**The grace window trades a little reuse detection for session stability.** For
ten seconds after a rotation, a stolen copy of the just-spent token would be
answered from this process's memory rather than reaching the API's replay check.
The alternative is a session that dies whenever a page issues two requests at
once, which the end-to-end run demonstrated it does.

**The single-flight is per process.** Two web tasks behind a load balancer can
still race, and the API will revoke the session when they do. It is also
observable in `next dev`, where Turbopack evaluates the module graph more than
once and the same twelve-request burst produces two refreshes and one reuse
detection; the production standalone build produces one and none. Filed as
issue #69: the fix is either a shared store for the in-flight map or a rotation
grace window on the API, and both are decisions bigger than this PR.

**The refresh cookie's max-age is a mirror, not a fact.** The API does not report
the refresh token's lifetime in its response, so the 14 days here mirrors
`AUTH_REFRESH_TOKEN_TTL`'s default. Being wrong in either direction degrades
safely: too long produces a failed refresh, which signs the user out cleanly; too
short signs them out early.

**Reversal.** If the API later sets cookies itself, the Route Handlers under
`app/api/auth` become thin relays and `lib/session/cookies.ts` mostly deletes —
the boundary this record establishes (Server Components read, handlers write,
clients go through a proxy) is unaffected. If the web app ever stops being
same-origin, the whole record is void and the token-in-body shape is right again.
Neither exit touches the endpoint catalogue in `lib/api/endpoints.ts`, which is
transport-agnostic on purpose.
