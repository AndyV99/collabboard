# 0009. Recovering a tenantless account with a password, not a scoped token

Date: 2026-08-09
Status: accepted

## Context

Registration is two transactions and structurally must be: a pre-tenant one
creating the global `users` row and its credential, and a tenant-scoped one
creating the organization and the owner membership. The tenant does not exist
until the second one's first statement runs, and `Store.WithoutTenant` and
`Store.WithTenant` are separate transactions by construction ([ADR
0002](0002-pre-tenant-identity-path.md), [ADR 0001](0001-tenant-isolation.md)).
A failure between them commits the first and loses the second, leaving an
account that authenticates and belongs to no organization — issue #34, and
documented in `Service.Register`'s doc comment since #8 rather than hidden.

Before this, such an account was stuck: the address is taken so re-registering
returns 409, nothing created an organization for an existing account, and the
pre-tenant path deliberately has no delete so it could not be cleaned up either.
The only exit was an operator with a `psql` prompt.

Issue #34 offered two designs and preferred the first: a
`POST /api/v1/organizations` endpoint, or a self-healing login. Self-healing
login was rejected for the reasons the issue gives — it hides the failure, and it
makes the endpoint that takes the most attack traffic a writer. That much was
settled. What the issue did not address is the question that actually shaped the
implementation.

**A subject with zero memberships cannot hold a token.** Not "does not"
— cannot:

- `Service.Login` returns `ErrNoOrganization` before it reaches `startSession`,
  so neither an access token nor a refresh token is ever issued;
- `Issuer.Issue` refuses a principal whose `TenantID` is `uuid.Nil`, because a
  token carrying the zero tenant would authenticate fine and then return empty
  results from every tenant-scoped query — "no data" where the truth is "no
  tenant";
- `Issuer.Verify` refuses a token whose `org` claim is the zero uuid;
- `internal/api`'s `principalFrom` and `PrincipalFromContext` refuse a principal
  without a tenant.

Each of those is load-bearing. The tenant claim is the entire boundary between
one organization's data and another's, and `internal/api/auth_middleware.go` is
the only place in the service that decides where a tenant comes from. So
"authenticated but tenantless" is not a state the token model can currently
represent, and the recovery endpoint has to be reachable by a subject in exactly
that state.

## Options

**Relax the tenant checks so a tenantless principal can exist.** Rejected
outright, and it is worth naming to record that it was considered. It would widen
the check that every other endpoint's isolation rests on, permanently and for
every route, to serve one endpoint that a given account calls at most once ever.
`auth_bola_test.go` attacks that check across eight vectors precisely because it
is the thing that must not move.

**A second token type, issued by login when the subject has zero memberships,
verified by a second middleware mounted only on this route.** This is the
shape most people reach for, and it is coherent: the narrow token would carry no
`org` claim, `Issuer` would not accept it, and the tenant-deriving path would be
untouched. Rejected on cost and on blast radius. It requires a second signing
audience, a second verifier, a second middleware and a second principal type —
three new things on the exact surface whose narrowness is the security property
— and it changes what `POST /auth/login` returns for a tenantless account from
`403` to a `200` carrying a token. The web app keys on that `403`
(`lib/auth/outcomes.ts` distinguishes it from a CSRF refusal by a private
header), so it is a cross-repo contract change as well.

**The endpoint takes the password.** An account in this state has exactly one
durable credential, and it is the password. The endpoint accepts it, verifies it
through the same function `Service.Login` uses, and creates the organization for
whatever subject that verification returns.

## Decision

`POST /api/v1/organizations` authenticates with email and password, mounted on
the unauthenticated route group beside register, login, refresh and logout.

Nothing in `auth_middleware.go` changed, and nothing needed to. The route never
derives a tenant, so it is not part of the tenant-derivation surface at all.

The credential check is `Service.verifyCredential`, extracted verbatim from
`Service.Login` and now called by both. Sharing the function rather than the
description is what keeps the anti-enumeration property true at both endpoints:
one argon2id derivation whatever is wrong, the same two pre-tenant lookups in the
same order, and the same error for an unknown address as for a wrong password.
The endpoint counts its attempts against the same two rate-limit budgets login
uses — which registration does *not*, despite its comment saying so (issue #73),
so the neighbours could not be relied on for it.

The organization is created by `Service.provisionOrganization`, which is
registration's second transaction extracted into a function that both callers
share. An organization created by the repair path is therefore identical to one
created by a registration that did not fail, owner membership included.

Scope is the zero-organization case only. An account that already has one gets
`409` and `ErrAlreadyHasOrganization`.

That check is sequential, not serialized. The membership read and the
provisioning are two transactions — necessarily, for the same structural reason
registration's two are — so two concurrent calls holding the correct password can
both observe zero memberships and both provision, leaving the account owning two
workspaces. Accepted: it needs the account's own password and genuine
concurrency, both organizations are correctly owned by that account, and no
boundary is crossed. A single-flight key in the rate limiter's Redis would close
it and was rejected, because a held or stuck key makes the repair path
*unavailable*, and an account that cannot recover at all is worse than an account
with a spare workspace. The guarantee is therefore "an account that already has
an organization is refused", not "an account can never come to have two".

## Consequences

**The blast radius is one route.** Because the subject is derived from a
credential rather than from a claim, this endpoint cannot be steered by a token,
a header, a path segment or a body field — there is no user id in the request
struct at all. `organizations_integration_test.go` attacks it the two ways that
remain: naming a victim's address without their password, and supplying a
victim's id alongside credentials that do verify. Neither moves the organization
off the account whose password was checked.

**A fifth endpoint an anonymous caller can reach, and a fourth that takes a
password.** That is the real cost, and it is stated rather than argued away. It
is bounded: the tighter unauthenticated body limit applies, the same rate limiter
applies, and an unknown address and a wrong password are answered identically, so
it is not a new address-existence oracle. The `409` is reachable only after a
correct password, so it discloses nothing to anyone who does not already hold the
credential.

**Token issuance did not spread.** The endpoint returns `201` with the
organization, not a session. `Principal.TenantID` is still written in exactly the
three places `internal/auth`'s package comment names, each of which derives it
from a membership it has just read. The client's next call is an ordinary login,
which now succeeds.

**The web app's copy is now wrong, in the good direction.**
`lib/auth/outcomes.ts` tells a tenantless user to "contact support and quote this
address so the workspace can be created". There is now a self-service path, and
nothing in `apps/web` calls it. Filed as a follow-up rather than folded in.

**Reversal, and where this gets revisited.** When an authenticated account should
be able to create *additional* workspaces (issue #86), that is a bearer-token
operation with a different authorization question: who may create a workspace,
how many, and — since an organization is a tenant — who may mint billable
tenants.

It cannot simply become a second mode on this same path, and it is worth
recording why so the next author does not start there. Gin's router panics at
construction on a duplicate method plus path, so
`unauthenticated.POST("/organizations", …)` and
`authenticated.POST("/organizations", …)` cannot both be mounted. That leaves two
shapes: parse the bearer token optionally *inside* the one handler on the
unauthenticated group, which moves an authentication decision out of
`requireAuth` and is the thing this ADR spent its length arguing against; or give
the authenticated operation its own path. The second is the one to prefer.

If a scoped pre-tenant token is ever wanted for other reasons — an
invite-acceptance flow, say — that is the point at which the second option above
should be reconsidered on its own merits, with this endpoint as one of several
callers rather than the only one.

**One property was given up.** `POST /api/v1/organizations` is the first
unauthenticated route not under `/auth/`, so the anonymous surface is no longer
identifiable by path prefix — a property both `router.go` and `internal/api`'s
file comments lean on when they enumerate what an anonymous caller can reach. The
path came from issue #34 rather than being invented here, and the mitigation is
that `router_test.go` now asserts the unauthenticated set exactly, so a route
added to that group is a failing test rather than a reading-comprehension
exercise.
