-- Create the role that owns the CollabBoard schema, and hand it the database.
--
-- This is the one step that genuinely cannot be a migration: the role the
-- migrations run as has to exist before they run, and creating it needs
-- privileges that role must not have. Run once per database, by the only
-- identity that has them — a superuser locally, the RDS master user in AWS.
--
--   psql -v ON_ERROR_STOP=1 \
--        -v owner_password="$(read the secret store)" \
--        -d collabboard \
--        -f apps/api/scripts/provision/bootstrap-owner.sql
--
-- Optional, when adopting a database whose objects are currently owned by
-- somebody else (a laptop volume created before issue #14, say):
--
--   -v previous_owner=collabboard
--
-- Idempotent: safe to re-run, and re-running rotates the owner's password.
--
-- See docs/adr/0006-database-role-provisioning.md for the whole role model, and
-- ADR 0001 for why the owner must not be the role the API connects as.

\if :{?owner_password}
\else
\warn 'bootstrap-owner.sql: pass the owner password, e.g. -v owner_password=...'
\quit
\endif

-- LOGIN because migrations connect as it. CREATEROLE because migrations 00001,
-- 00004 and 00005 create collabboard_app, collabboard_identity and
-- collabboard_credentials; if those are pre-provisioned out of band instead,
-- CREATEROLE can be dropped, but the owner still needs ADMIN OPTION on all
-- three — see the note at the bottom of this file.
--
-- Everything else is withheld, and the withholding is the point:
--
--   NOSUPERUSER   a superuser is exempt from row-level security, full stop
--   NOBYPASSRLS   the same exemption under a different name
--   NOCREATEDB    this role owns one database; it has no business making more
--   NOREPLICATION a logical replication slot is a copy of every tenant's rows
--
-- CREATEROLE is not the escalation it looks like. PostgreSQL 16 forbids a
-- CREATEROLE role from granting SUPERUSER, BYPASSRLS or REPLICATION, and from
-- granting CREATEDB unless it holds CREATEDB itself, so this role cannot
-- manufacture a way out of its own constraints.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_owner') THEN
        CREATE ROLE collabboard_owner
            LOGIN
            CREATEROLE
            NOSUPERUSER
            NOBYPASSRLS
            NOCREATEDB
            NOREPLICATION;
    END IF;
END
$$;

-- Unconditional, so that a role created by an earlier version of this script,
-- or by hand, converges on the same shape. This statement is why the script
-- needs a privileged identity: NOSUPERUSER, NOBYPASSRLS, NOCREATEDB and
-- NOREPLICATION can only be *asserted* by a caller that holds them, which is
-- exactly why the migrations cannot assert them and check instead.
ALTER ROLE collabboard_owner
    LOGIN
    CREATEROLE
    NOSUPERUSER
    NOBYPASSRLS
    NOCREATEDB
    NOREPLICATION;

ALTER ROLE collabboard_owner PASSWORD :'owner_password';

-- Ownership of the database, so that `CREATE SCHEMA auth` in migration 00005
-- works and so that the owner can be dropped only by deliberately reassigning
-- first. current_database() cannot appear in ALTER DATABASE directly, hence the
-- format().
DO $$
BEGIN
    EXECUTE format('ALTER DATABASE %I OWNER TO collabboard_owner', current_database());
END
$$;

-- Ownership of schema public, so the owner can create the tables, functions and
-- policies the migrations define, and so it can grant CREATE on the schema to
-- collabboard_identity and collabboard_credentials for the length of the four
-- ALTER FUNCTION ... OWNER statements each of them needs.
--
-- PostgreSQL 15 and later leave schema public owned by pg_database_owner, which
-- would work by implication once the database is owned above. Naming the owner
-- outright is a line of SQL against an implication three catalogs deep.
ALTER SCHEMA public OWNER TO collabboard_owner;

-- Already the default from PostgreSQL 15 onward. Restated because a database
-- restored from a pre-15 dump carries the old, open grant, and an unprivileged
-- role that can create objects in public can shadow a table name a SECURITY
-- DEFINER function resolves.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

-- Administrative rights over the three roles the migrations manage, for any of
-- them that already exists.
--
-- On a fresh database none do, and this is a no-op: the migrations create them,
-- and PostgreSQL 16 gives a CREATEROLE role ADMIN OPTION on anything it creates.
-- It matters in the two cases where they exist already — Terraform provisioned
-- them out of band, or this database was migrated before issue #14 by some other
-- role. In both, the owner has no membership at all, and every ALTER ROLE, every
-- GRANT ... TO CURRENT_USER and every DROP OWNED BY in the migration chain fails
-- without one. Doing it here rather than leaving it as a note in a comment is
-- the difference between that being a deploy-time surprise and a solved problem.
--
-- INHERIT FALSE is the load-bearing word. The default for a role grant is to
-- inherit, and inheriting collabboard_identity means every policy attached
-- `TO collabboard_identity` — all of them `USING (true)` — applies to the owner
-- ambiently, so a plain SELECT as the migration role returns the entire global
-- user directory. Migration 00006 does the same thing for the grants it makes
-- itself; this covers the ones made here, which it cannot revoke because a role
-- may only revoke what it granted.
--
-- SET TRUE is what 00004 and 00005 actually need: ALTER FUNCTION ... OWNER
-- requires the caller to be able to SET ROLE to the new owner. ADMIN OPTION is
-- what every ALTER ROLE and DROP ROLE in the chain needs.
DO $$
DECLARE
    role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['collabboard_app', 'collabboard_identity', 'collabboard_credentials']
    LOOP
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = role_name) THEN
            EXECUTE format(
                'GRANT %I TO collabboard_owner WITH ADMIN OPTION, INHERIT FALSE, SET TRUE',
                role_name);

            RAISE NOTICE 'granted collabboard_owner ADMIN + SET (not INHERIT) on %', role_name;
        END IF;
    END LOOP;
END
$$;

-- Adoption. Only runs when -v previous_owner=... is passed.
--
-- For a database whose objects are owned by somebody else — the case every
-- laptop is in, because the compose stack's bootstrap superuser migrated it.
-- Ownership has to move, or FORCE ROW LEVEL SECURITY still applies to the wrong
-- role and migration 00006 refuses to run.
--
-- Deliberately not REASSIGN OWNED BY. That is the one-line version and it does
-- not work here: the bootstrap superuser also owns the pinned system catalogs,
-- so PostgreSQL rejects it with "cannot reassign ownership of objects owned by
-- role X because they are required by the database system". It is also far
-- broader than wanted — it would move objects that were never CollabBoard's.
-- This walks `public` and `auth` and moves exactly what it finds there.
--
-- Objects owned by collabboard_identity and collabboard_credentials are left
-- alone, because the filter is on the previous owner: the four identity
-- functions and three credential functions must keep the owners migrations 00004
-- and 00005 gave them, or the whole SECURITY DEFINER design collapses.
\if :{?previous_owner}
\echo 'adopting objects owned by' :'previous_owner'
SELECT set_config('provision.previous_owner', :'previous_owner', false);

DO $$
DECLARE
    previous_owner text := current_setting('provision.previous_owner');
    obj            record;
    moved          integer := 0;
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = previous_owner) THEN
        RAISE EXCEPTION 'previous_owner "%" does not exist', previous_owner;
    END IF;

    FOR obj IN
        SELECT n.nspname AS schema_name, c.relname AS object_name, c.relkind AS kind
        FROM pg_catalog.pg_class c
        JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname IN ('public', 'auth')
          AND c.relowner = previous_owner::regrole
          -- Indexes, TOAST tables and the like follow their parent; only
          -- independently owned kinds are listed.
          AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
        ORDER BY 1, 2
    LOOP
        EXECUTE format(
            CASE obj.kind
                WHEN 'S' THEN 'ALTER SEQUENCE %I.%I OWNER TO collabboard_owner'
                WHEN 'v' THEN 'ALTER VIEW %I.%I OWNER TO collabboard_owner'
                WHEN 'm' THEN 'ALTER MATERIALIZED VIEW %I.%I OWNER TO collabboard_owner'
                ELSE 'ALTER TABLE %I.%I OWNER TO collabboard_owner'
            END, obj.schema_name, obj.object_name);

        moved := moved + 1;
    END LOOP;

    FOR obj IN
        SELECT p.oid::regprocedure AS signature
        FROM pg_catalog.pg_proc p
        JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname IN ('public', 'auth')
          AND p.proowner = previous_owner::regrole
        ORDER BY 1
    LOOP
        EXECUTE format('ALTER FUNCTION %s OWNER TO collabboard_owner', obj.signature);

        moved := moved + 1;
    END LOOP;

    FOR obj IN
        SELECT n.nspname AS schema_name
        FROM pg_catalog.pg_namespace n
        WHERE n.nspname IN ('public', 'auth')
          AND n.nspowner = previous_owner::regrole
    LOOP
        EXECUTE format('ALTER SCHEMA %I OWNER TO collabboard_owner', obj.schema_name);

        moved := moved + 1;
    END LOOP;

    RAISE NOTICE 'moved % object(s) from % to collabboard_owner', moved, previous_owner;
END
$$;
\endif

-- What this script deliberately does not do.
--
-- It does not provision collabboard_app, collabboard_identity or
-- collabboard_credentials: the migrations create all three, and creating them is
-- why the owner holds CREATEROLE. If a deployed environment would rather
-- Terraform own their lifecycle, the GRANT ... WITH ADMIN OPTION block above is
-- what makes that work.
--
-- It does not set the app role's password. That comes from the secret store by
-- way of POSTGRES_PASSWORD and `api provision`; see ADR 0006.

SELECT
    r.rolname            AS role,
    r.rolsuper           AS is_superuser,
    r.rolbypassrls       AS bypasses_rls,
    r.rolcreaterole      AS can_create_roles,
    r.rolcanlogin        AS can_log_in,
    pg_catalog.pg_get_userbyid(d.datdba) AS database_owner,
    pg_catalog.pg_get_userbyid(n.nspowner) AS public_schema_owner
FROM pg_catalog.pg_roles r
CROSS JOIN pg_catalog.pg_database d
CROSS JOIN pg_catalog.pg_namespace n
WHERE r.rolname = 'collabboard_owner'
  AND d.datname = current_database()
  AND n.nspname = 'public';

-- Every membership the owner holds, so that "administers but does not inherit"
-- is visible at the end of a run rather than something to go and check.
SELECT
    r.rolname        AS over_role,
    m.grantor::regrole::text AS granted_by,
    m.admin_option   AS admin,
    m.inherit_option AS inherits,
    m.set_option     AS can_set_role
FROM pg_catalog.pg_auth_members m
JOIN pg_catalog.pg_roles r ON r.oid = m.roleid
WHERE m.member = 'collabboard_owner'::regrole
ORDER BY 1, 2;
