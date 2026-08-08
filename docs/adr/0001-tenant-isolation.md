# 0001. Tenant isolation via Postgres row-level security

Date: 2026-08-08
Status: accepted

## Context

CollabBoard is multi-tenant — organizations own projects, boards and cards — and
broken access control across an org boundary is the failure mode that matters
most for this app (see the vault's `Standards/Security Practices.md`: tenant
isolation is *the* OWASP item here). The isolation model determines the schema
shape, the migration strategy, the connection pool, and the signature of every
function in `internal/store`, so it has to be decided before any data-layer code
exists. Reversing it later touches all four.

Constraints from the decided stack: a single RDS Postgres instance, sqlc + pgx
(hand-written SQL, generated Go, a pooled connection with a prepared-statement
cache), goose migrations embedded in the binary, one ECS Fargate service. The
expected shape is many small tenants, not a handful of large contractually
isolated ones.

**Row-level security** — one schema, `tenant_id` on every tenant-scoped table,
RLS enabled and `FORCE`d, policies keyed on `current_setting('app.tenant_id')`,
set per transaction with `SET LOCAL`. Isolation is enforced by the database
underneath the application, so a query that forgets its tenant predicate returns
nothing rather than everything. Migrations are O(1): one goose run per deploy at
any tenant count. Pooling is unaffected — `SET LOCAL` is transaction-scoped, so a
connection returned to the pgx pool carries no tenant state, and the statement
cache stays valid because the SQL text and the relations it binds to never
change. Blast radius is the worst of the three: one table shipped without a
policy, or one code path that opens a transaction without setting the GUC, leaks
across every tenant simultaneously.

**Schema-per-tenant** — one Postgres schema per tenant. Its real advantage is
that it fails *closed*: a wrong `search_path` produces "relation does not exist",
an error rather than someone else's cards. Against that: migrations become
O(tenants) and a failure partway through leaves the fleet in mixed states;
tenant signup becomes DDL on a request path; the catalog grows by
(tables × tenants), degrading planning and catalog autovacuum long before row
counts would. It also interacts badly with this specific stack — pgx caches
prepared statements keyed by SQL text, so identical sqlc-generated SQL executed
under different `search_path`s can bind to the wrong tenant's tables unless the
cache is disabled or connections are pinned per tenant. The fix costs the shared
pool, which is what keeps one small Fargate service in front of RDS viable.
Cross-tenant queries (billing rollups, admin views) degrade to UNION over N
schemas.

**Database-per-tenant** — strongest isolation, and the only option offering
per-tenant backup/restore and a true blast radius of one. Rejected at this
scale: a pool per database, migrations across N connections, per-tenant
infrastructure cost, and no practical way to answer a product-wide question.
This is justified when tenants are few, large and contractually isolated — the
opposite of this product.

## Decision

Use Postgres row-level security. A single schema; `tenant_id uuid NOT NULL` on
every tenant-scoped table; `ENABLE`/`FORCE ROW LEVEL SECURITY` with policies
matching `current_setting('app.tenant_id')`; the tenant set per transaction via
`SET LOCAL app.tenant_id`.

## Consequences

**What every query in `internal/store` must do.** Every tenant-scoped query runs
inside a transaction that has already executed `SET LOCAL app.tenant_id = $1`.
Concretely, `internal/store` exposes one helper — `WithTenant(ctx, tenantID, fn)`
— that acquires a pooled connection, begins a transaction, issues the `SET
LOCAL`, and hands `fn` a sqlc querier bound to that transaction. Handlers never
receive the pool, and no tenant-scoped query is issued outside that helper. This
ADR is the justification for that rule in `CLAUDE.md`. `SET LOCAL` rather than
`SET` is load-bearing: it is reset at commit or rollback, so a pooled connection
cannot carry one tenant's context into another tenant's request. Queries may
still carry `tenant_id` in their `WHERE` clause to help the planner use the
index, but correctness must not depend on the author remembering to — the policy
is the guarantee, the predicate is an optimization.

**The superuser/owner trap.** RLS is bypassed by superusers, by roles with
`BYPASSRLS`, and by the table's owner unless `FORCE ROW LEVEL SECURITY` is set.
The API therefore must not connect as the RDS master user or as the role that
owns the tables. Migrations run as the owner (goose, at deploy time); the API
connects as a dedicated `collabboard_app` role that is not a superuser, lacks
`BYPASSRLS`, and owns nothing — it holds table-level grants only. Getting this
wrong makes every policy silently decorative while all tests still pass, so the
integration harness should assert both `rolsuper = false` / `rolbypassrls =
false` for the app role and that every tenant-scoped table has RLS forced with a
policy attached.

**Easier / harder.** Easier: one migration per deploy regardless of tenant count,
tenant provisioning is an `INSERT`, and cross-tenant reporting is ordinary SQL
run through an explicit, auditable elevated path. Harder: isolation is invisible
at the call site, so it only stays real if tested — asserted as an invariant over
all tables rather than remembered per table — and "why did this return zero rows"
gains one more suspect. Follow-up work: issue #5 (schema + RLS migrations), #6
(sqlc + tenant-context store helpers), #7 (Testcontainers harness, including a
test that a query issued without tenant context returns nothing).

**Reversal.** If this proves wrong — most plausibly because a customer requires
physical isolation or per-tenant restore — the exit is database-per-tenant for
those tenants only, running the same application code. The code change is
contained: `WithTenant` stops issuing `SET LOCAL` and instead routes to a
per-tenant pool, because every tenant-scoped query already funnels through that
one function. That containment is the second reason the helper exists. The
expensive parts are the data move (export by `tenant_id`, import, with a short
write freeze per tenant) and the permanent ops surface (N pools, N migration
targets, per-tenant connection limits). Rough cost at this codebase's size: days
of engineering for the routing change plus per-tenant migration effort — bad but
bounded, and it stays bounded only while the "no raw tenant-scoped query outside
`internal/store`" rule holds.
