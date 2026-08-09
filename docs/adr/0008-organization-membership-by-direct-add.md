# 0008. Organization membership by direct add, not by invitation

Date: 2026-08-09
Status: accepted

## Context

The demo the project exists to show is *"sign up → create board → invite
teammate → move a card and see it update live in a second browser context"*.
`GET /api/v1/members` lists an organization's members; nothing adds one, so the
third step has no endpoint and the headline realtime feature cannot be
demonstrated with two real users. Issue #61.

The pieces are already there and unused. [ADR 0002](0002-pre-tenant-identity-path.md)
built `identity_resolve_user_id_by_email` in the pre-tenant identity path
*specifically* so that an admin could resolve an invited address to an account
that may live entirely outside their tenant's visibility — the query file says
so in as many words:

> Inviting a user who already has an account, possibly in an organization the
> inviting admin cannot see. Pre-tenant because looking outside the current
> tenant's visibility is the entire point.

And `CreateMembership` in `query.sql` is described as "the second half of an
invite, and the half that is *not* pre-tenant". So the shape of the thing was
anticipated. The decision this ADR records is the one that was left open: does
the second half happen when the *admin* asks, or when the *invitee* accepts?

## Options

**An invitation record the invitee accepts.** A new tenant-scoped
`invitations` table — tenant, address, role, a hash of a single-use token, an
expiry, a status — a `POST /api/v1/invitations` that creates one, and a
`POST /api/v1/invitations/accept` that the invitee calls with the token. More
honest about consent: nobody is put into an organization without an act of
their own.

Rejected, and the reason is structural rather than a matter of effort.

*The accept step cannot be built without widening the pre-tenant door.* At the
moment the invitee accepts, they are by definition **not** a member of the
inviting tenant. So the transaction that accepts has to (a) read an invitation
row belonging to a tenant the caller is not in, and (b) insert a membership into
that tenant. Both are exactly what the RLS policies from
[ADR 0001](0001-tenant-isolation.md) forbid: `invitations` would carry
`tenant_id` and its policy would key on `current_tenant_id()`, and
`memberships_tenant_isolation`'s `WITH CHECK` refuses an insert naming any other
tenant. The only ways out are a new `SECURITY DEFINER` function in the identity
path that resolves a token to a tenant and writes a membership, or an
`invitations` table deliberately left outside RLS. The first widens the narrow
door — and widens it with the one thing ADR 0002 is most careful about, a
*write* into a tenant-scoped table from a function whose caller has no tenant.
The second puts a table holding every organization's pending invitations outside
the isolation model that the whole data layer is built on.

*And the consent it buys is not deliverable.* An invitation is only a consent
mechanism if the token reaches the invitee by a channel the inviter does not
control. This service has no mailer, and none is planned before the demo. An
invitation whose token comes back in the inviting admin's own HTTP response is
not consent; it is a direct add with two round trips and a table.

**Direct add of an account that already exists.** The admin submits an address;
if an account exists, a membership is created in the admin's own tenant
immediately. Both halves are already built: the pre-tenant resolve and the
tenant-scoped insert.

**Direct add that also creates the account** — i.e. `identity_create_user` for
an unregistered address, so the invited person gets an account with no password.
Rejected as a superset of the disclosure problem with none of the benefit: it
would let one admin manufacture rows in the global `users` table for addresses
they merely typed, and the resulting account would be unusable until a password
reset flow that does not exist.

## Decision

Direct add, of an account that already exists.

`POST /api/v1/members` takes `{"email": ..., "role": ...}`, resolves the address
through `Store.WithoutTenant` under the existing `ReasonInviteLookup`, and
inserts a membership through `Store.WithTenant` scoped to the caller's org
claim. **No migration, no new query, no new pre-tenant function, and no change
to generated code**: `sqlc diff` exits 0 on this change. The narrow door is used
exactly as ADR 0002 specified and is not one function wider.

The consequences of skipping consent are bounded rather than ignored:

- **Only an existing account can be added.** This path never calls
  `identity_create_user`, so an address with no account cannot be pulled into an
  organization at all — it is a 404. The person being added has, at minimum,
  already chosen to have an account on this service.
- **A membership is all that is granted.** The added account gets `member` by
  default, and a `member` may not add anyone, so an addition cannot cascade.
- **Nothing is taken away.** Membership is additive: the added account keeps
  every organization it was already in, and `GET /api/v1/me` shows the new one
  alongside them.
- **Every attempt is logged** with the actor, the tenant and a stable outcome
  label — and never with the address.

The exit, if consent stops being an acceptable trade, is the invitation table;
it is a purely additive change (a new table, a new endpoint pair, and the
pre-tenant function the accept step needs) that does not have to unpick this
one.

### Who may add, and what they may grant

`owner` and `admin`. A `member` may not — the issue is explicit that "any
member" is not an acceptable default, and since `member` is the role every added
account gets, allowing it would make one addition enough to grow an organization
without limit.

The role granted is bounded by the actor's own:

| actor    | may grant           |
| -------- | ------------------- |
| `owner`  | `member`, `admin`   |
| `admin`  | `member`            |
| `member` | nothing             |

Nobody may grant `owner` through this endpoint. Transferring ownership has
different consequences from adding a colleague and deserves its own operation
and its own review; refusing it here is a 400, and it is refused as a property
of the endpoint rather than of the caller, so no owner discovers that theirs
almost could.

**The role comes from `memberships`, not from the token's `role` claim.** The
claim is minted at login and re-derived at most once per access-token lifetime,
so a demoted admin would carry it for the rest of that lifetime. `AddMember`
reads the row inside the tenant transaction instead, and does so *again* in the
transaction that performs the INSERT, so "the actor held the role at the instant
the row was written" is true rather than "held it a moment earlier".

### The order of operations, and why it is the order

1. authorize, in a tenant-scoped transaction;
2. resolve the address, through the pre-tenant door;
3. re-authorize and insert, in a second tenant-scoped transaction.

Three transactions rather than one because step 2 travels the other door and the
two doors are separate transactions by construction. Authorization is first so
that a caller who may not add never causes a directory lookup — which keeps the
pre-tenant audit log free of lookups nobody was entitled to make.

Duplicates are refused by the unique index on `(tenant_id, user_id)`: the INSERT
is attempted and `23505` becomes a 409. Reading first and inserting second would
leave a window where two concurrent requests both see no row; the index has no
such window, and the issue's "never a duplicate membership row" is therefore a
property of the schema rather than of the handler.

## Consequences

**What the caller learns.** Exactly one bit per request: *this address has an
account*. That bit is inherent — the operation succeeds only if the account
exists — so it is bounded rather than eliminated:

- the asker must be authenticated **and** an owner or admin, so the directory is
  not reachable anonymously or by a plain member;
- authorization precedes the lookup, so an unauthorized caller learns nothing
  and triggers nothing;
- one exact address per request. The pre-tenant door exposes no pattern query
  and no query that returns a set of users, and adding one takes a migration, a
  grant and a new reason (ADR 0002). Bulk enumeration is one authenticated,
  logged HTTP request per address;
- the refusal body is a fixed sentence, identical for every unregistered
  address: no user id, no display name, nothing about any organization. The
  success body carries the membership just created and the normalised address
  the caller typed — not the display name, and not the account's other
  organizations;
- every attempt is logged under `auth.member.add_refused` /
  `auth.member.added` with the actor and the tenant, and never with the address,
  so a burst of probing is visible without the log becoming the result.

**This is not the widest way to ask that question.** `POST /api/v1/auth/register`
already answers it for an *unauthenticated* caller: a duplicate address is a
409, a trade `internal/api/auth.go` accepts deliberately because the alternative
needs the mailer that does not exist. A channel that requires an account, an
organization and an owner or admin role cannot be the weakest link while that
one is open. `TestWhatAnAttackerLearnsByAddingAddressesInBulk` asserts both
halves side by side, so if registration ever stops disclosing it, the comparison
this decision rests on fails loudly instead of silently.

**What is not rate limited.** `internal/auth`'s `Limiter` is keyed to login and
counts per account and per client address; nothing budgets member additions.
That is a real residual channel — an owner can probe at HTTP speed — and it is
filed rather than folded in, because the right fix covers registration too and
is bigger than this endpoint. See #73, which also records that the existing
comment claiming registration *is* rate limited is wrong.

**Reversal.** Cheap in the direction that matters. Adding invitations later does
not have to remove this endpoint: a direct add and an accepted invitation
produce the same `memberships` row, so the invitation flow can be layered on and
the direct add restricted or removed at that point. What this ADR spends is the
consent, not the schema.
