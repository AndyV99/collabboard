# 0003. Password verifiers: argon2id in the application, sha256 in the database

Date: 2026-08-08
Status: accepted

## Context

[ADR 0002](0002-pre-tenant-identity-path.md) built a deliberately narrow door
for the identity operations that cannot run inside a tenant-scoped transaction.
Its closing section names what it left for issue #8:

> It cannot ... read a column it was not granted — a password hash added by #8 is
> unreachable until that grant is edited.

Issue #8 has to store a password and check it, and login runs before any tenant
exists, so the credential has to be reachable from the pre-tenant path. The
one-line version of this change is `GRANT SELECT (password_hash) ON users TO
collabboard_identity`, and it would quietly make every one of the four existing
identity functions — and every future one — a candidate hash-disclosure
endpoint. The narrowness ADR 0002 spent a migration on would be gone in a single
`GRANT`.

Two decisions are entangled here and this record makes both:

1. **Which KDF, and where it runs.** The issue asks for argon2id, or bcrypt with
   a documented cost.
2. **What the database stores, and who can read it.** This is the part that
   interacts with ADR 0002.

Constraints from the existing design: `collabboard_app` is the only role the API
connects as; it is not a superuser, holds no `BYPASSRLS` and owns nothing. The
pool is small (`POSTGRES_MAX_CONNS` defaults to 10) and shared by every request.
`users` carries a table-level grant to `collabboard_app` from migration 00002,
so *any* column added to `users` is readable by the serving role — RLS restricts
which rows, not which columns.

## Options

**bcrypt inside the database, via pgcrypto.** One `SECURITY DEFINER` function
taking an email and a candidate password and returning a verified user id;
`crypt(candidate, stored) = stored` inside the body. This satisfies "the hash
never leaves the database" in the most literal way and needs no new application
code. Rejected on two counts, either of which is enough.

First, it puts password-hashing CPU on the database. bcrypt at cost 12 is
roughly 250ms of a Postgres backend holding a pooled connection. With ten
connections, ten concurrent logins stall every other query in the service for a
quarter of a second, and the cost of mounting that is ten HTTP requests. Rate
limiting per IP does not help against a distributed attempt. The one component
that cannot be scaled horizontally is the worst place to put deliberately
expensive work.

Second, it sends the plaintext password to Postgres on every login, as a bind
parameter. That is one `log_min_duration_statement` plus `log_parameter_max_length`
away from plaintext passwords in the database logs, and it puts them in front of
anyone with `pg_stat_activity` while the statement runs.

**argon2id in the application, encoded hash stored and readable through a
function.** The conventional shape: store the PHC-encoded argon2id string, hand
it to the application through a definer function, compare in Go. Simple, no new
crypto, and everyone recognises it. Its weakness is precisely the thing ADR 0002
is built around: anything that can call that function obtains crackable material
for every account it names. The threat model that motivated the separate role
and the four-function surface — "a query travelling this path should be able to
do one thing, not one thing plus a class of things" — argues against handing out
hashes if there is an alternative that does not.

**argon2id in the application, and grant the identity role the column.** The
one-line version. Rejected in the Context above: it widens the existing door
rather than opening a narrower one, and it makes the four existing functions
capable of something they were specifically built not to be capable of.

**argon2id in the application, sha256 of the derived key in the database.** The
application asks for the KDF parameters (salt, memory, iterations, parallelism,
key length — all public by construction), derives the key with argon2id, and
sends the key to a function that compares `sha256(sent)` against the stored
column. Two round trips instead of one, and a stored value that is neither the
password nor a hash the application can verify against.

## Decision

argon2id in the application; `sha256()` of the derived key in the database;
credential storage in a schema the serving role cannot enter, owned by a role of
its own.

**The KDF.** argon2id via `golang.org/x/crypto/argon2`, at RFC 9106's second
recommended parameter set — 19 MiB of memory, 2 iterations, 1 lane, 32-byte
output — as the configurable default. Per-row salts of 16 bytes from
`crypto/rand`. The parameters are stored per row, not assumed, so raising them
later re-hashes on next login instead of locking every existing account out.
Concurrency is bounded by a semaphore in `internal/auth` so that the memory cost
is a queue rather than an OOM.

**What is stored.** `auth.user_credentials.verifier` is `sha256(argon2id(password,
salt, params))`. This is the shape SCRAM (RFC 5802) uses and the shape PostgreSQL
stores its own `scram-sha-256` passwords in: `StoredKey = H(ClientKey)`. Nothing
here is a new primitive — argon2id and SHA-256, composed the way an existing
standard composes them.

The property that buys: **what is stored cannot be replayed.** An attacker
holding a dump has `sha256(key)` and would need its preimage to authenticate, so
a leaked verifier is not a credential. Cracking it costs exactly what cracking a
plain argon2id hash costs — one argon2id derivation per guess, plus a SHA-256 —
so nothing is given up in exchange. And because the application only ever
*sends* a derived key and never *receives* the stored one, there is no function
anywhere that returns crackable material. `credentials_test.go` asserts that from
the catalog: no `SECURITY DEFINER` function in `public` has a result column named
`verifier`.

**Where it lives.** A new schema, `auth`, containing one table. `collabboard_app`
holds no `USAGE` on it. That is a stronger boundary than a row-level policy: RLS
filters rows out of a table you can see, while no schema `USAGE` means the table
does not resolve at all. It is also the answer to the table-level grant on
`users` — a credential column on `users` would have been readable by the serving
role the moment it existed, RLS notwithstanding, because RLS constrains rows and
that grant covers every column.

**Who can read it.** A third role, `collabboard_credentials`: `NOLOGIN`,
`NOSUPERUSER`, `NOBYPASSRLS`, owning three `SECURITY DEFINER` functions in
`public` and holding column privileges on exactly one table in `auth`. It holds
**no privilege of any kind in schema `public`** — it cannot read an email, a
display name, a membership or a card — which is why its functions take a user id
rather than an email. Symmetrically, `collabboard_identity` gets nothing in
`auth`.

So the honest answer to "how much did the door widen" is: **the identity door did
not widen at all.** A second, strictly narrower door was cut next to it, and the
two roles cannot reach each other's data. That is asserted at runtime, not
described: `credentials_test.go` builds a definer function owned by
`collabboard_identity` that reads `auth.user_credentials`, and one owned by
`collabboard_credentials` that reads `public.users`, grants the app role
`EXECUTE` on both, and calls them. Postgres refuses both.

**What the path cannot do.** No `UPDATE` grant, no `DELETE` grant, no `UPDATE` or
`DELETE` policy, and an `INSERT` with no `ON CONFLICT`. This path can create a
credential once and can never change or remove one. Password change and password
reset are separate features that need their own function, their own grant and
their own review — which is the same friction ADR 0002 chose deliberately, applied
to the operation most worth gating.

## Consequences

**Login costs three pre-tenant transactions.** Resolve the email to a user, read
the parameters, verify. They are separate transactions on purpose: the ~80ms
derivation happens between the second and the third, and holding a pooled
connection across it would reintroduce exactly the pool-exhaustion failure that
disqualified bcrypt-in-the-database. Login is not a hot path; three round trips
on it is the right trade.

**Two reasons were added to the closed set** — `ReasonPasswordParams` and
`ReasonVerifyPassword` — and `IdentityQuerier` went from four methods to seven.
Both are visible, deliberate widenings of the *Go* surface, and both are asserted
by tests that had to be edited to accommodate them. That friction is ADR 0002
working as intended: the list exists so that adding to it requires saying why.

**The `bytea` comparison is not constant time.** PostgreSQL compares `bytea` with
a length check and a `memcmp`. Extracting a byte of `verifier` through that
timing would mean resolving a few nanoseconds through an HTTP request, a Redis
round trip for the rate limiter, an ~80ms argon2id derivation and a network hop
to Postgres — and the value recovered would be a SHA-256 digest, not a password.
Accepted deliberately. If it ever stops being acceptable, the fix is a
constant-time comparison in the function body, which is a migration and no
application change.

**A derived key crosses the wire to Postgres.** It is not the password and not
the stored value, but it is authentication material for one account for as long
as the password stands, so the database connection has to be TLS in any deployed
environment (`POSTGRES_SSLMODE=require` or stronger). That is already true for
every other reason; it is noted here because this makes it load-bearing rather
than merely correct. It is strictly better than the bcrypt-in-database option,
which puts the plaintext password on the same wire.

**Argon2id memory is an availability surface.** 19 MiB per concurrent
derivation, and login does one derivation whether or not the account exists —
which is the anti-enumeration requirement, so it cannot be skipped. Bounded by a
semaphore (`AUTH_ARGON2_MAX_CONCURRENT`, default 4) and by the login rate
limiter. The failure mode under load is queueing and 429s rather than an OOM
kill.

**Reversal.** If argon2id's memory cost turns out to be the wrong shape for the
deployment — a small Fargate task where 19 MiB × concurrency does not fit — the
exit is to lower the parameters, which is a config change that re-hashes each
account on its next login because parameters are stored per row. If the
verifier-not-hash split turns out to be more cleverness than it is worth, the
exit is a migration that adds a function returning the stored column and a Go
change to compare there instead; the application already owns the argon2id
implementation, so no credential has to be re-derived. Neither exit is expensive.
The decision that *would* be expensive to reverse is where the data lives and who
owns it, which is why that half is the conservative half.
