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

The query set is deliberately representative, not exhaustive — enough to
exercise the mechanism across the interesting boundaries (a plain tenant-scoped
list, a child collection reached by parent id, a join from tenant-scoped
`memberships` to global `users`, and a write). Later work adds queries next to
the feature that needs them.

## Adding a query

1. Add it to `query.sql` with a `-- name:` annotation.
2. Run `sqlc generate` from `apps/api/`.
3. Add an alias in `types.go` for any new row or params struct.
4. Commit the generated files — they are checked in so that a build never needs
   sqlc installed, and so the diff shows what the generated SQL actually became.

## What this package deliberately cannot do

Identity operations that happen *before* a tenant is known cannot run through
`WithTenant`:

- login by email
- "which organizations do I belong to?"
- inviting a user who already has an account in another organization

`users` is global and its policy is derived from `memberships` — a row is
visible only if a membership joins it to the current tenant — so there is no
tenant to scope these to, and a tenant-scoped transaction cannot even create a
user (the membership that would make it visible cannot exist before the user
does). That is [issue #13](https://github.com/AndyV99/collabboard/issues/13), and
it belongs in a **separate, named, auditable entry point on `Store`**, not in a
widened `WithTenant`.

Nothing here forecloses it: the restriction is that `gen` is unimportable from
outside `internal/store`, which says nothing about how many doors
`internal/store` itself opens. A second method — pool-bound rather than
transaction-bound, handing back a deliberately narrow interface over the two or
three global queries it is allowed to run — is additive.

## Tests

Split by build tag, because the two loops have very different costs.

```bash
go test ./internal/store/...                    # ~ms, no Docker
go test -tags=integration ./internal/store/...  # ~4s, brings up Postgres
```

| File | Tag | What it covers |
|---|---|---|
| `store_unit_test.go` | — | argument checks that stop a wiring mistake reaching a database |
| `sqlcconfig_test.go` | — | the generated code stays at the unimportable path |
| `store_test.go` | `integration` | `WithTenant` / `InTenant` through the generated querier |
| `isolation_test.go` | `integration` | every tenant-scoped table, both directions, all four verbs |
| `identity_test.go` | `integration` | that the connection under test is actually `collabboard_app` |
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
