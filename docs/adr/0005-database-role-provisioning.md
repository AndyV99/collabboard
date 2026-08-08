# 0005. Database role provisioning: a non-superuser schema owner, and app credentials from configuration

Date: 2026-08-08
Status: accepted

## Context

ADR 0001 chose row-level security for tenant isolation and named the trap that
comes with it: RLS is bypassed by superusers, by roles holding `BYPASSRLS`, and
by a table's owner unless `FORCE ROW LEVEL SECURITY` is set. It concluded that
migrations run as the owner and the API connects as `collabboard_app`, which is
none of those things.

Half of that was implemented. `collabboard_app` is real, tested, and narrow. The
owner was never provisioned at all: migrations ran as `collabboard`, the compose
stack's bootstrap role, which is `rolsuper = t` and `rolbypassrls = t`. So the
role that installed every policy was exempt from all of them, `FORCE` was never
exercised by the role it exists for, and nothing noticed — the integration
harness migrated as the same superuser.

Two consequences had accumulated by the time issue #14 was picked up.

**The migration chain could not be applied by a correct owner.** Migrations
00001, 00004 and 00005 each ran `ALTER ROLE ... NOSUPERUSER NOBYPASSRLS
NOCREATEDB ... NOREPLICATION`. PostgreSQL only lets a role change one of those
attributes if it *holds* it — asserting the negative counts as changing it —
so every one of those statements required a superuser. Verified against
PostgreSQL 16.14:

```
ERROR:  permission denied to alter role
DETAIL:  Only roles with the SUPERUSER attribute may change the SUPERUSER attribute.
```

The same applies to `BYPASSRLS`, `CREATEDB` and `REPLICATION`. `LOGIN`,
`NOCREATEROLE`, `INHERIT` and `PASSWORD` are settable by an ordinary
`CREATEROLE` role; the rest are not. `CREATE ROLE x NOBYPASSRLS` is fine for any
creator, because PostgreSQL only checks the attribute when it is being changed
on an existing role.

**The app role's password came from a checked-in file.** Migration 00001 creates
`collabboard_app` without a password on purpose — a credential in a versioned
migration can never be rotated. The gap was filled by
`apps/api/scripts/dev/set-app-role-password.sql`, which set it to the literal
`dev`, with a comment saying deployed environments would do something else.
Nothing did the something else, and there was no mechanism for one to.

There is no Terraform, no secret store and no deployed environment in this
repository yet, so this decision has to be one that does not depend on any of
them existing.

## Decision

**A dedicated `collabboard_owner` role owns the schema and applies migrations.**
`LOGIN CREATEROLE NOSUPERUSER NOBYPASSRLS NOCREATEDB NOREPLICATION`. It owns the
database, schema `public`, schema `auth` and every table, so
`FORCE ROW LEVEL SECURITY` applies to it.

`CREATEROLE` is not the escalation it looks like: PostgreSQL 16 forbids a
`CREATEROLE` role from conferring `SUPERUSER`, `BYPASSRLS` or `REPLICATION`, and
from conferring `CREATEDB` unless it holds it. The owner therefore cannot
manufacture a way out of its own constraints. It holds `CREATEROLE` because
migrations 00001, 00004 and 00005 create the other three roles.

**Provisioning it is a script, not a migration** —
`apps/api/scripts/provision/bootstrap-owner.sql`, run once per database by a
superuser locally or the RDS master user in AWS. This is the one step that
genuinely cannot be a migration: the role migrations run as must exist before
they run, and creating a role that must not be a superuser requires privileges
that role must not have. The compose stack runs the same file from
`/docker-entrypoint-initdb.d`, and the integration harness runs both the hook and
the file, so dev, CI and the documented deploy step are the same artifact.

**Migrations verify the attributes they can no longer set.** 00001, 00004 and
00005 state the negative attributes on `CREATE ROLE`, set the two the owner may
set (`LOGIN`/`NOLOGIN`, `NOCREATEROLE`, `NOINHERIT`) with `ALTER ROLE`, and check
the remaining four in a `DO` block that raises with the offending attribute
named.

**A new migration, 00006, refuses to run as an exempt role** — and
`internal/migrate` makes the same check before goose applies anything, so the
answer to "I ran it as the wrong role" is "nothing happened" rather than "five
migrations landed owned by the wrong role". Both use
`row_security_active('public.users')` rather than an attribute lookup.

**00006 also drops `INHERIT` from the memberships the owner holds.** 00004 and
00005 grant the migration role membership in `collabboard_identity` and
`collabboard_credentials`, because `ALTER FUNCTION ... OWNER` requires the caller
to be able to `SET ROLE` to the new owner. A plain `GRANT` carries `SET` *and*
`INHERIT`. `WITH INHERIT FALSE` keeps what is needed and drops what is not.

**The app role's password comes from `POSTGRES_PASSWORD` via `api provision`.**
One new subcommand, one new package (`internal/provision`), and the
checked-in dev SQL file is deleted. The same value the API authenticates with is
the value written into the database, so there are not two things that have to
agree.

## Consequences

**The local path gains one command and loses one.** `docker compose up -d` now
creates `collabboard_owner` through an init hook; `api migrate up` runs as it;
`api provision` replaces piping a SQL file through `psql`. An existing `pgdata`
volume predates the hook, so it needs either `docker compose down -v` or the
documented adoption one-liner — `bootstrap-owner.sql -v previous_owner=collabboard`,
which moves object ownership without destroying data.

**`REASSIGN OWNED BY` is not the adoption mechanism.** It is the obvious answer
and it does not work: a bootstrap superuser also owns pinned system catalogs, so
PostgreSQL rejects it outright. The script walks `public` and `auth` and moves
what it finds there, which is also narrower than `REASSIGN OWNED BY` would be.

**Why `row_security_active` and not `rolsuper`/`rolbypassrls`.** The attribute
check is wrong in both directions. It passes for a role that is neither but does
not own the tables, and — the case that matters — it passes for the RDS master
user, which is not a superuser and holds no `BYPASSRLS`; its power comes from
`rds_superuser` membership. That is precisely the identity this ADR exists to
stop migrating as. `row_security_active` answers the question directly, and also
catches ownership without `FORCE`, an inherited membership in a role a permissive
policy names, and `SET row_security = off`.

**What the owner can still do, and why that is accepted.** It owns every table,
so it can issue `ALTER TABLE ... NO FORCE ROW LEVEL SECURITY` at any time, and it
can `SET ROLE collabboard_identity`. No database privilege prevents either. What
constrains it is that only reviewed migrations connect as this role, and that
both are deliberate statements rather than ambient capability — which is the
whole difference `INHERIT FALSE` buys.

**The blast radius of one environment variable shrank.** Before this work, the
migration role could read the entire global user directory with no tenant context
set, because it inherited `collabboard_identity` and the pre-tenant policies are
`USING (true)`. Measured, not theorised: it saw every row of `users`,
`organizations` and `memberships`. After, it sees zero rows of all seven
tenant-scoped tables. `POSTGRES_USER` being pointed at the migration credentials
is the single most likely misconfiguration in this system, and this is the
difference between it leaking the directory and it leaking nothing.

**What the app-role password decision does *not* solve.** There is no secret
store, so `api provision` reads `POSTGRES_PASSWORD` from the process
environment. In ECS that is a `secrets:` entry resolving a Secrets Manager ARN
before the container starts, which is where the injection belongs — outside the
process, so the value is not in the image, the task definition or this
repository. There is deliberately no `SecretSource` interface: a second
implementation would have nothing to do, and an abstraction with one
implementation and no second candidate is a guess about the future dressed as a
seam.

What remains genuinely blocked on infrastructure, and is not pretended otherwise:
the Terraform that creates the secret, grants the task role read access to it,
runs `bootstrap-owner.sql` against a fresh RDS instance, and wires
`api migrate up && api provision` as a pre-deploy task. Rotation is likewise a
contract rather than an implementation — change the secret, run `api provision`,
roll the tasks, in that order, because changing the password invalidates it for
every task still holding the old one. No amount of code in this repository makes
that not a deploy.

**Reversal.** Cheap. The owner role is created by one script and named by one
environment variable; pointing `POSTGRES_MIGRATION_USER` back at a superuser and
deleting migration 00006 plus the preflight would restore the previous
behaviour in an afternoon. The reason not to is that the previous behaviour was
a schema whose isolation was never exercised by the role that installed it.
