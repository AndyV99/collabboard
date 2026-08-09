# internal/store

The Postgres data layer, and the only place in the service that talks to
tenant-scoped tables.

## The rule

> Every tenant-scoped query runs inside a transaction that has already set
> `app.tenant_id`, and the only way to open one is `Store.WithTenant`.

```go
err := s.WithTenant(ctx, tenantID, func(ctx context.Context, q store.Querier) error {
    projects, err := q.ListProjects(ctx)   // no tenant argument anywhere
    ...
    return err
})
```

or, when the callback produces a value:

```go
board, err := store.InTenant(ctx, s, tenantID, func(ctx context.Context, q store.Querier) (store.Board, error) {
    return q.GetBoard(ctx, boardID)
})
```

`fn` returning an error rolls the transaction back. The connection goes back to
the pool on every path, including a panic.

## Why

Tenant isolation is enforced by Postgres row-level security rather than by
application predicates — see [ADR 0001](../../../../docs/adr/0001-tenant-isolation.md).
Every policy is written against `current_tenant_id()`, which reads the
`app.tenant_id` GUC and returns `NULL` when it is unset. `NULL` propagates
through `tenant_id = current_tenant_id()` as `NULL`, which a policy treats as
false, so a transaction that never set the GUC sees **zero rows rather than
every row**.

That is a good failure mode, but it is only a failure mode. The isolation is
real only while every query actually runs inside a transaction that set the GUC.
This package is that guarantee.

`SET LOCAL` rather than `SET` is the load-bearing detail: it is reverted at
commit or rollback, so a connection handed back to the pool carries no tenant
state and cannot serve the next request with the previous request's tenant. The
helper issues it as `SELECT set_config('app.tenant_id', $1, true)` — `SET LOCAL`
takes a literal and cannot be parameterised, and `set_config`'s third argument
(`is_local`) gives identical semantics with the value travelling as a bind
parameter instead of being interpolated into SQL.

## How the rule is enforced, not just documented

Three layers, in decreasing order of strength:

1. **The compiler.** sqlc generates into `internal/store/internal/gen`. Go's
   internal-package rule makes that importable only from within
   `internal/store`, so no other package can call `gen.New(pool)` and get a
   querier bound to the pool. Trying produces a build failure:

   ```
   internal/api/handler.go:8:2: use of internal package
   github.com/AndyV99/collabboard/apps/api/internal/store/internal/gen not allowed
   ```

   `sqlcconfig_test.go` asserts the generated code stays at that path, since the
   guarantee comes from *where* it is generated and nothing else in the build
   would notice if that changed.

2. **`Store` exposes no pool.** The `*pgxpool.Pool` is an unexported field with
   no accessor. `cmd/api` builds the pool, hands it to `store.New`, and hands
   the `*Store` on; nothing downstream ever holds the pool.

3. **A lint rule** (`depguard`, in `.golangci.yml`) rejects importing `pgx` or
   `database/sql` from `internal/api` and `internal/realtime`. This closes the
   remaining gap: a package that is passed a pool anyway and writes raw SQL.

The types callers need — `Querier`, `Project`, `Board`, `CreateProjectParams`
and so on — are re-exported from `types.go` as aliases. An alias gives the name
without the import, so handlers can declare variables of these types while
`gen.New` stays unreachable. Adding a query means adding one alias line; that
friction is deliberate, so the set of database types the rest of the service can
see is a decision rather than a side effect of what sqlc happened to emit.

## Query conventions

No query in `query.sql` takes a `tenant_id` parameter.

- **Reads**: the policy supplies `tenant_id = current_tenant_id()`. The planner
  can still use the tenant-leading indexes for it, because `current_tenant_id()`
  is `STABLE`, so repeating the predicate by hand buys nothing and invites a
  caller to pass a tenant that disagrees with the transaction's.
- **Writes**: the tenant comes from `current_tenant_id()` in the `INSERT` itself.
  There is no argument to get wrong, which makes the `WITH CHECK` half of the
  policy unreachable in practice rather than merely unviolated.

`GetMembership` is the one query whose *shape* is worth a note: it takes a user
id and no tenant, so the policy answers "is this user a member of the
transaction's tenant". The WebSocket hub asks it on an interval for every live
connection, because a connection authorized once outlives the check that
authorized it — see `internal/realtime/README.md`.

`GetUser` (issue #75) has the same shape and the same reason. `users` is global,
so its policy is *derived*: `users_visible_via_membership` shows a row only when
a membership joins it to the current tenant. A tenant-scoped `GetUser` therefore
returns a colleague, or no row — never a stranger — which is why `GET /me` can
report the caller's own email and display name without going anywhere near the
pre-tenant door. It is a narrowing of `ListMembers`, not a new reach:
`ListMembers` already returns the same two columns for every member.
`store_test.go`'s `TestGetUserIsBoundedByTheDerivedUsersPolicy` asserts all
three cases — own row, the user shared between both tenants, and the other
tenant's member by real primary key.

The query set started out deliberately representative rather than exhaustive —
enough to exercise the mechanism across the interesting boundaries (a plain
tenant-scoped list, a child collection reached by parent id, a join from
tenant-scoped `memberships` to global `users`, and a write). Issue #47 added the
rest of the board hierarchy next to the feature that needed it, which is the
pattern for everything after it.

### The ordering queries

`MoveCard` and `MoveColumn` are the only queries here that are not
straightforward, and two things about them are worth knowing before editing
either. Both are argued in [ADR 0004](../../../../docs/adr/0004-card-ordering.md).

- **`(lower + upper) * 0.5`, never `(lower + upper) / 2`.** `numeric`
  multiplication is exact; division picks a bounded result scale and rounds to
  it, so halving with `/` stops making progress after ~53 nested subdivisions of
  one gap and starts returning a value equal to one of the bounds — two cards
  with the same rank.
- **The caller holds `LockColumn` (for cards) or `LockBoard` (for columns)**
  before any statement that allocates a position, including `CreateCard` and
  `CreateColumn`. The lock is on the *parent*, never on the siblings: one lock
  per operation means two concurrent moves cannot each hold one and wait for the
  other's. It is what makes ranks within a column distinct by construction.
  `internal/api/cards.go` documents the statement order and why it is that order.

Both moves return `NeedsRebalance`, which is the query saying the new rank's
scale has passed 100 decimal places. The caller renumbers the column with
`RebalanceColumnCards` / `RebalanceBoardColumns` while it still holds the lock.

### The deletes that return a row

`DeleteCard` and `DeleteColumn` are `DELETE ... RETURNING *` rather than
`:execrows`, and `DeleteBoard` is not. The asymmetry is deliberate: issue #45
addresses realtime events to a *board*, and after a card or column has been
deleted its board id exists nowhere else — whereas a board's own id is in the
request path. "No row" still means the same thing it meant as a zero count, and
`internal/api` still turns it into the same 404 for an id that never existed and
for one belonging to another tenant.

## Adding a query

1. Add it to `query.sql` with a `-- name:` annotation.
2. Run `sqlc generate` from `apps/api/`.
3. Add an alias in `types.go` for any new row or params struct.
4. Commit the generated files — they are checked in so that a build never needs
   sqlc installed, and so the diff shows what the generated SQL actually became.

Adding a *pre-tenant* query is deliberately harder, and should be: see
"The other door" below.

## The other door: `WithoutTenant`

Identity operations that happen *before* a tenant is known cannot run through
`WithTenant`. `users` is global and its policy is derived from `memberships` —
a row is visible only if a membership joins it to the current tenant — so with
no tenant set, the app role sees zero rows in `users`, `memberships` and
`organizations`. That is the correct fail-closed behaviour, and it is also why
these operations are *impossible* rather than merely awkward. The same is
true of the credential half added by issue #8: `auth.user_credentials` lives in
a schema the app role holds no `USAGE` on, so it cannot name the table at all,
with or without a tenant.

```go
user, err := store.WithoutTenantValue(ctx, s, store.ReasonLogin,
    func(ctx context.Context, q store.IdentityQuerier) (store.IdentityUser, error) {
        return q.FindUserByEmail(ctx, email)
    })
```

| Query | Reason it cannot be tenant-scoped |
|---|---|
| `FindUserByEmail` | login: the input is an email; nothing has claimed an organization yet |
| `ResolveUserIDByEmail` | inviting an existing account: the point is to look *outside* the inviting tenant's visibility |
| `ListUserOrganizations` | the org switcher: the question is "which tenants", so none can be current |
| `CreateUser` | the `users` `WITH CHECK` policy needs a membership that cannot exist before the user row does |
| `PasswordParams` | login: the KDF parameters, read before any organization has been claimed |
| `VerifyPassword` | login: the comparison, performed inside the database |
| `CreatePassword` | registration: same transaction as `CreateUser`, and before any tenant exists |

Everything else belongs in `WithTenant`. Adding a member to an organization,
for instance, is *not* pre-tenant: `CreateMembership` lives in `query.sql` and
runs scoped to the admin's own tenant. An invite is deliberately split across
both doors for exactly that reason.

### Why this cannot become a general escape hatch

Four independent things have to be true for a query to travel this path, and no
single edit makes them all true.

1. **The compiler.** `identity_query.sql` generates into
   `internal/store/internal/identitygen`, a *different* package from the
   tenant-scoped one. `IdentityQuerier` and `Querier` share no methods:

   ```
   internal/api/handler.go:10:11: q.ListProjects undefined
   (type store.IdentityQuerier has no field or method ListProjects)
   ```

   Both are under a nested `internal`, so neither constructor is importable
   outside `internal/store`.

2. **Function grants.** Every one of those methods is a call to a
   `SECURITY DEFINER` function created in
   [`migrations/00004_pre_tenant_identity.sql`](../../migrations/00004_pre_tenant_identity.sql)
   or [`migrations/00005_auth_credentials.sql`](../../migrations/00005_auth_credentials.sql).
   `collabboard_app` holds `EXECUTE` on exactly those seven and can read nothing
   without a tenant otherwise.

3. **Table grants, and two owners rather than one.** The four identity functions
   run as `collabboard_identity`, which holds *column-level* privileges on
   `users`, `memberships` and `organizations`, **no privilege of any kind** on
   `projects`, `boards`, `columns` or `cards`, and nothing in schema `auth`. The
   three credential functions run as `collabboard_credentials`, which holds
   column privileges on `auth.user_credentials` and **no privilege of any kind
   in schema `public`**. Neither role can reach the other's data: the identity
   path cannot read a password verifier, and the credential path cannot read an
   email. A definer function that reached across fails with `permission denied`
   — asserted at runtime in `credentials_test.go`, which builds both of those
   functions and calls them. Neither role can log in, neither is a superuser,
   neither holds `BYPASSRLS`, neither owns a table, and `collabboard_app` is a
   member of neither, so the serving role cannot `SET ROLE` to either.

4. **A named reason.** Every call passes an `IdentityReason` from a closed set
   (a struct with an unexported field, so the constants are the only values
   any package can name), and every call is logged with it. The zero value is
   rejected.

So widening the path is not a Go method away. It takes a migration that adds a
function, a grant, possibly a privilege no pre-tenant role has ever held, and a
new reason — each a line in a diff that says what it is doing. Issue #8 is the
worked example: it needed all four, and it chose a *new role* over a new grant
to the existing one, so the identity role's reach is byte-for-byte what ADR 0002
left it. See [ADR 0003](../../../../docs/adr/0003-password-verifier-storage.md).

**The one thing it cannot enforce:** `ListUserOrganizations` does not authorize.
Callers must pass the id of the already-authenticated subject; passing one from
a request body would turn it into a membership-disclosure endpoint. That is #8's
responsibility, as are rate limiting and the timing characteristics of a login
lookup.

See [ADR 0002](../../../../docs/adr/0002-pre-tenant-identity-path.md) and
[issue #13](https://github.com/AndyV99/collabboard/issues/13).

## Tests

Split by build tag, because the two loops have very different costs.

```bash
go test ./internal/store/...                    # ~ms, no Docker
go test -tags=integration ./internal/store/...  # ~4s, brings up Postgres
```

| File | Tag | What it covers |
|---|---|---|
| `store_unit_test.go` | — | argument checks that stop a wiring mistake reaching a database |
| `sqlcconfig_test.go` | — | both generated packages stay at the unimportable path |
| `pretenant_unit_test.go` | — | the width of the pre-tenant door, including a real `go build` proving a tenant-scoped query does not compile against it |
| `store_test.go` | `integration` | `WithTenant` / `InTenant` through the generated querier |
| `isolation_test.go` | `integration` | every tenant-scoped table, both directions, all four verbs |
| `identity_test.go` | `integration` | that the connection under test is actually `collabboard_app` |
| `pretenant_test.go` | `integration` | each pre-tenant query, including one user's memberships spanning two tenants and an invite end to end |
| `pretenant_narrow_test.go` | `integration` | that the pre-tenant path cannot reach tenant-scoped data, from the catalog *and* by building a definer function that tries |
| `credentials_test.go` | `integration` | that the two pre-tenant roles cannot reach each other's data, that the app role cannot enter schema `auth`, and a password round trip |
| `main_test.go`, `testdb_test.go` | `integration` | container lifecycle and fixtures |

The harness is `internal/testsupport/pgtest`: a Postgres container on a random
port, migrated by the real `internal/migrate` code, with a two-tenant fixture
that includes **one user belonging to both organizations**. That shared user is
what makes the assertions mean something — without it, "tenant A sees only its
own members" would also hold if the query returned the whole table.

Queries under test run as the serving role (`collabboard_app`: not a superuser,
no `BYPASSRLS`, owns nothing), which is the only identity the policies apply to.
`identity_test.go` asserts that from inside the connection rather than trusting
the DSN — connecting as the owner or a superuser would make every isolation
assertion in this package pass while proving nothing, and that is the single
most likely way for this suite to go quietly vacuous.

Fixtures are seeded as the migration role, because seeding is precisely what the
policies forbid — see the previous section.

`isolation_test.go` also asks the catalog which tables exist and fails if the
matrix does not cover one of them, so a table added by a future migration cannot
slip through untested.
