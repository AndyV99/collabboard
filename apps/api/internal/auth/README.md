# internal/auth

Registration, login, sessions, and the principal an authenticated request acts
as.

## The rule

> The tenant id comes from the `org` claim of a token this service signed, and
> from nowhere else.

`store.WithTenant` sets `app.tenant_id` to whatever it is handed, and Postgres
serves that tenant faithfully. [ADR 0001](../../../../docs/adr/0001-tenant-isolation.md)
is explicit that the row-level security layer is *isolation, not
authorization* — so the boundary between one customer and another's boards is
drawn here, by what this package puts in `Principal.TenantID`.

It is populated in exactly three places, all in `service.go`, all from
memberships resolved for the **authenticated subject** through the pre-tenant
path:

| Where | From |
|---|---|
| `Login` | the subject's memberships; the first by name becomes the active tenant |
| `Refresh` | the same lookup, re-run — a revoked membership stops working here |
| `SwitchOrganization` | the same lookup, filtered to the requested id; a non-member gets 403 |

Nothing reads a tenant from a header, a path segment, a query parameter or a
request body, and `internal/api` has no route or field that could carry one.
`SwitchOrganization` is the single place a client-supplied organization id
exists, which is why it is the place the BOLA test attacks hardest.

## Endpoints

| Method | Path | Auth | Notes |
|---|---|---|---|
| `POST` | `/api/v1/auth/register` | — | creates the user, its credential, and a first organization it owns |
| `POST` | `/api/v1/auth/login` | — | rate limited; 401 with one body for every failure |
| `POST` | `/api/v1/auth/refresh` | — | rotates the refresh token, re-checks the membership |
| `POST` | `/api/v1/auth/logout` | — | the refresh token is the credential; 204 even for an unknown one |
| `POST` | `/api/v1/auth/organization` | bearer | switch active organization; 403 for a non-member |
| `GET` | `/api/v1/me` | bearer | the principal and the organizations it could act in |
| `GET` | `/api/v1/members` | bearer | tenant-scoped, through `store.WithTenant` |

`/api/v1/members` exists to make "the tenant flows from the token into the data
layer" testable end to end. Board and card CRUD are out of scope for issue #8.

## Credentials

argon2id in the application, `sha256()` of the derived key in the database. The
full reasoning is [ADR 0003](../../../../docs/adr/0003-password-verifier-storage.md);
the short version:

- The KDF runs here, not in Postgres. Deliberately expensive work on a pooled
  connection turns a handful of concurrent logins into a service-wide stall,
  and the database is the one component that cannot scale horizontally.
- What is stored is `sha256(argon2id(password))`, so a dump cannot be replayed
  to the verify function and no function returns crackable material. This is the
  SCRAM `StoredKey = H(ClientKey)` shape (RFC 5802), which is also how
  PostgreSQL stores its own `scram-sha-256` passwords.
- Cost parameters are stored per credential, so raising them re-derives an
  account on its next login instead of locking everyone out.
- Concurrency is bounded (`AUTH_ARGON2_MAX_CONCURRENT`). Login performs a
  derivation whether or not the account exists, so ~19 MiB per in-flight login
  is an availability surface; the bound turns a burst into a queue and then into
  errors, rather than an OOM kill.

## Sessions

| | Access token | Refresh token |
|---|---|---|
| Form | HS256 JWT, claims carry `sub`, `org`, `role`, `sid` | 256 random bits, opaque |
| Lifetime | 15 minutes (`AUTH_ACCESS_TOKEN_TTL`) | 14 days (`AUTH_REFRESH_TOKEN_TTL`) |
| Validated by | signature only — nothing is consulted | a lookup in Redis |
| Revocable | **no** | **yes**, immediately |

That pairing is the design: the access token is short-lived *because* it cannot
be revoked, and the refresh token can be revoked *because* it is a lookup.

Refresh tokens are stored under `auth:refresh:<sha256 of the token>`, so a Redis
snapshot is not a bag of live sessions. Each session also stores
`auth:session:<sid>` pointing at its current token. Every refresh mints a new
token; presenting a superseded one is a replay, and the answer is to revoke the
whole session — including whichever token is live at that moment, which may be
in the thief's hands or the victim's. A false positive costs one login; a false
negative costs the account.

The verifier pins `HS256` as an allow-list rather than reading the algorithm
from the token header, and asserts issuer, audience and the presence of `exp`.

## Not revealing whether an address exists

Every failing login does the same work in the same order: rate-limit both
budgets, look the address up, read KDF parameters (the account's own, or a
stand-in derived from the signing secret with HKDF), run **exactly one**
argon2id derivation, and ask the database to compare. Unknown address, wrong
password and "account exists but has no password" are one error and one 401
with one body.

`TestLoginDoesTheSameWorkWhateverIsWrong` asserts the derivation count and the
sequence of pre-tenant reasons, which is a stronger and less flaky claim than a
stopwatch; `TestLoginTakesComparableTimeForAnUnknownAddress` in the integration
suite carries a loose wall-clock ratio as a backstop.

Registration is the deliberate exception: a duplicate address gets 409. Always
answering 201 needs a mailer this service does not have, and without one a user
could not tell a typo from a taken address. Login — the endpoint an attacker
enumerates at scale — gives nothing away.

## Rate limiting

Two budgets, one fixed window, both incremented on every attempt including
successful ones:

- **per account** (default 5 / 15 min), keyed by an HMAC of the normalised
  address so a Redis dump is not a list of who has tried to log in;
- **per client address** (default 30 / 15 min), loose because a NAT puts many
  real users behind one IP.

The limiter **fails open** if Redis is unreachable, and logs when it does. The
argument is narrow and is written down in `ratelimit.go`: the same Redis backs
the session store, so a login cannot succeed while it is down — failing closed
would trade nothing for a misleading "too many attempts". That reasoning stops
holding the moment anything else can issue a session.

`ClientIP()` is the peer address: the router calls `SetTrustedProxies(nil)`,
because Gin trusts `X-Forwarded-For` from anyone by default and that makes the
per-address budget one header away from useless. It will need the load
balancer's subnet once one exists.

## Logging

One event per outcome, with `event` set to a closed-set name
(`auth.register.success`, `auth.login.failed`, `auth.refresh.reuse_detected`,
`auth.switch_organization.denied`, …). Failures log a reason category and the
client address; they do **not** log the address being attempted, because a log
full of addresses is a credential-stuffing target list with timestamps.

Never logged: passwords, derived keys, verifiers, access tokens, refresh tokens.
Asserted, not assumed — `TestNothingSensitiveReachesTheLogs` and its integration
counterpart grep the emitted log for each of those values, with a control that
fails if the logger is silent.

## Tests

```bash
go test ./internal/auth/... ./internal/api/...                    # no Docker
go test -tags=integration ./internal/api/...                      # Postgres + Redis containers
```

| File | Tag | What it covers |
|---|---|---|
| `internal/api/auth_bola_test.go` | — | **the BOLA test**: every channel a request could name a tenant through, plus a deliberately vulnerable router proving the assertions detect one |
| `internal/api/auth_middleware_test.go` | — | no / malformed / tampered / wrong-key / expired token, and the WWW-Authenticate challenge |
| `internal/auth/service_test.go` | — | the derivation-count anti-enumeration claim, rate limiting, refresh, membership revocation, log hygiene |
| `internal/auth/token_test.go` | — | alg=none, wrong key, rewritten payload, wrong issuer/audience, missing exp |
| `internal/auth/session_test.go` | — | rotation, reuse detection, revocation, expiry |
| `internal/config/auth_test.go` | — | the signing secret is required outside development and long enough |
| `internal/api/auth_integration_test.go` | `integration` | register → login → authenticated request, tenant isolation across two real organizations, refresh revocation, 429 and Retry-After, timing |
| `internal/store/credentials_test.go` | `integration` | the database half: neither pre-tenant role can reach the other's data |
