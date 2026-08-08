# 0002. Pre-tenant identity via owned SECURITY DEFINER functions

Date: 2026-08-08
Status: accepted

## Context

[ADR 0001](0001-tenant-isolation.md) put tenant isolation in Postgres row-level
security: every tenant-scoped table carries `tenant_id`, RLS is enabled and
`FORCE`d, policies key on `current_setting('app.tenant_id')`, and `internal/store`
exposes one helper — `WithTenant` — that sets the GUC per transaction. `users` is
the one table outside that shape: a person can belong to several organizations,
so identity cannot be owned by one tenant. It is global, with a *derived* policy
— a row is visible only if a membership joins it to the current tenant.

That combination makes three operations impossible rather than awkward, because
at the moment they run there is no tenant to set:

1. **Login by email.** The input is an address; nothing has claimed an
   organization yet.
2. **"Which organizations do I belong to?"** The answer spans tenants, so no
   tenant can be current while it is asked.
3. **Creating a global user**, including for an invite to an address that
   already has an account elsewhere. The `WITH CHECK` half of the `users` policy
   requires a membership joining the row to the current tenant, and that
   membership cannot exist before the row it references does.

`collabboard_app` is not a superuser, holds no `BYPASSRLS` and owns nothing, so
every policy applies to it. With `app.tenant_id` unset, `current_tenant_id()` is
`NULL` and `users`, `memberships` and `organizations` all return zero rows. This
is the fail-closed behaviour ADR 0001 wants — and it is also why *something* has
to be granted before authentication can work at all. #8 (auth) is blocked on it.

The risk is not that the feature is hard. It is that whatever gets built becomes
a general-purpose RLS bypass that later code reaches for out of convenience,
quietly undoing ADR 0001. ADR 0001 already anticipated this: "cross-tenant
reporting is ordinary SQL run through an explicit, auditable elevated path". This
ADR decides what that path is.

## Options

**A dedicated `WithoutTenant` in `internal/store`, and nothing at the database
level.** Cannot work on its own. There is no privilege to fall back on: the app
role genuinely cannot read `users` without a tenant, so a Go-only door returns
zero rows. It would have to be paired with one of the options below, and the
question is which.

**A second login role with its own pool.** Give `collabboard_identity` `LOGIN`,
grants on the three identity tables, its own policies, and point a small second
pool at it. Real isolation, and the issue lists it. Rejected on two counts.
First, it adds a credential — a second DSN, a second secret to rotate, a second
thing to get wrong in Terraform — for a role whose entire job is four queries.
Second, and worse, the narrowness would live only in Go: that role could
`SELECT * FROM users` unrestricted, so "only these four operations" would be a
convention enforced by code review rather than by the database.

**Widening the `users` policy with a GUC.** A permissive policy like
`USING (current_setting('app.identity_lookup', true) = 'on')` needs no new role
and no new pool. Rejected: it attaches to `collabboard_app`, the same role every
tenant-scoped request uses, and the key is a session setting any statement can
set. It converts a structural boundary into a flag, on the one role whose
constraint is load-bearing.

**`SECURITY DEFINER` functions owned by a `NOLOGIN` role.** One function per
operation, returning only the columns that operation needs, owned by a role that
exists solely to own them, with `EXECUTE` granted to `collabboard_app` and
revoked from `PUBLIC`.

## Decision

`SECURITY DEFINER` functions, owned by a `NOLOGIN` role.

Migration `00004_pre_tenant_identity.sql` creates `collabboard_identity`:
`NOLOGIN`, `NOSUPERUSER`, `NOBYPASSRLS`, owning nothing but four functions. It
holds *column-level* privileges on `users`, `memberships` and `organizations`,
and no privilege of any kind on `projects`, `boards`, `columns` or `cards`. RLS
stays enabled and forced on all three identity tables; the new policies carry
`TO collabboard_identity`, and PostgreSQL never evaluates a role-listed policy
for any other role, so `collabboard_app`'s view of those tables is unchanged.

The four functions are `identity_find_user_by_email`,
`identity_resolve_user_id_by_email`, `identity_list_user_organizations` and
`identity_create_user`. Each has `SET search_path = pg_catalog, public` — without
it a caller could point `users` at a table they control and have the function
operate on it as its owner. `EXECUTE` is revoked from `PUBLIC` (the default on a
new function) and granted to `collabboard_app` alone.

In Go, `Store.WithoutTenant(ctx, reason, fn)` is the only way to call them. It
opens a transaction, explicitly clears `app.tenant_id`, and hands `fn` an
`IdentityQuerier` generated from `identity_query.sql` into
`internal/store/internal/identitygen` — a different package from the
tenant-scoped querier, with no methods in common. `reason` is drawn from a closed
set of four and logged at every call.

Reassigning ownership needs the migration role to be a member of
`collabboard_identity`, and it keeps that membership. Handing it back would look
tidier and buy nothing — the migration role owns every table, so it can already
run `ALTER TABLE users NO FORCE ROW LEVEL SECURITY` — while breaking rollback,
since `DROP ROLE` needs the `ADMIN OPTION` the revoke would give away. The
boundary that matters is that `collabboard_app` cannot assume the identity role,
which is asserted directly.

## Consequences

**Four independent guards, and no single edit defeats them.** The compiler
(`q.ListProjects` does not exist on `IdentityQuerier`, and neither generated
package is importable outside `internal/store`); the `EXECUTE` grant (four
functions, no more); the table grants (the definer role cannot read tenant data
even from inside a function it owns); and the named reason. Widening the path
takes a migration, a grant, and a fifth reason — each a visible line in a diff.

**The claim is tested, not asserted.** `pretenant_narrow_test.go` builds the
exact thing this design fears — a `SECURITY DEFINER` function owned by
`collabboard_identity` that counts rows in a tenant-scoped table — grants the app
role `EXECUTE`, and calls it. Postgres answers `42501: permission denied for
table projects`. The table list comes from the catalog, so a table added by a
future migration is covered the moment it exists. Two more tests assert the
executable-function set and the identity role's attributes, so the door's width
is checked from the database and not only from Go.

**What this path can and cannot see.** It can read `users` by exact email, read
`memberships` and `organizations` by user id, and insert a `users` row. It cannot
update or delete a user, cannot enumerate the directory (no function takes a
pattern or returns a set of users), cannot read a column it was not granted — a
password hash added by #8 is unreachable until that grant is edited — and cannot
touch a tenant-scoped table at all.

**Easier / harder.** Easier: no new credential, no second pool, no config
surface; #8 consumes four typed methods. Harder: adding a pre-tenant capability
now needs a migration, so the friction is real and deliberate. `sqlc` also does
not expand a `RETURNS TABLE` into output columns, so each query names its columns
in the `FROM` clause and casts them — mechanical, and explained in
`identity_query.sql`.

**What this deliberately does not decide.** `identity_list_user_organizations`
does not authorize: callers must pass the already-authenticated subject's id.
Rate limiting, lockout and the timing characteristics of a login lookup belong to
#8. The shape here neither forces nor forecloses a constant-time response — it
reports "no such account" as no row, which a handler can normalise — but it does
mean #8 has to decide deliberately rather than inherit a decision.

**Reversal.** If a pre-tenant capability ever needs more than a function can
express, the exit is the second-role-and-pool option above, and the code change
is contained for the same reason ADR 0001's is: every pre-tenant query already
funnels through `WithoutTenant`. The expensive part would be the new credential
and its rotation, not the Go.
