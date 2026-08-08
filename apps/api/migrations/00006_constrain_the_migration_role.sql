-- Constrain the role the migrations themselves run as.
--
-- See issue #14 and docs/adr/0005-database-role-provisioning.md.
--
-- Every other migration constrains a role the application uses. This one
-- constrains the role that applies migrations, which until now was whatever the
-- deploy happened to connect as. Two parts, both about the same role:
--
--   1. refuse to run at all as a role row-level security is not enforced
--      against
--   2. hold the memberships 00004 and 00005 need without inheriting the
--      privileges that come with them
--
-- It is a new migration rather than an edit to the earlier ones for the reason
-- forward migrations exist: 00001 through 00005 have already been applied to
-- every database that exists, and goose will never run them again there. Only a
-- version those databases have not seen can tell them anything.

-- +goose Up

-- # Part one: refuse to migrate as an exempt role
--
-- ADR 0001 calls it the superuser/owner trap: RLS is bypassed by superusers, by
-- BYPASSRLS roles, and by a table's owner unless FORCE ROW LEVEL SECURITY is
-- set. Migrations 00001 through 00005 all assume they are being applied by a
-- role in the third category — an owner that FORCE brings back under its own
-- policies. Until this file, nothing checked. A database migrated by the
-- compose stack's bootstrap superuser looked identical to one migrated by a
-- properly constrained owner: same tables, same policies, same grants, and a
-- silently exempt owner.
--
-- A test cannot close that gap, because the mistake is made by whoever runs the
-- deploy rather than by whoever writes the schema. The check has to travel with
-- the schema and run at the moment the wrong role is used.
--
-- ## Why row_security_active and not a pg_roles lookup
--
-- The obvious check is `rolsuper = false AND rolbypassrls = false`. It is the
-- wrong one, in both directions:
--
--   * it passes for a role that is neither, but does not own the tables and so
--     has no policies applied to it for an entirely different reason
--   * it passes for the RDS master user, which is not a superuser and holds no
--     BYPASSRLS — its power comes from rds_superuser membership instead. That
--     is precisely the role this issue exists to stop migrating as, and an
--     attribute check would wave it through
--
-- row_security_active() answers the question actually being asked: is RLS being
-- enforced against *me*, right now, for this table. It accounts for superuser,
-- for BYPASSRLS, for ownership without FORCE, for inherited membership in a
-- role a permissive policy names, and for a session that has set
-- row_security = off. `users` is the subject because it is the table whose
-- policy ADR 0001 leans on hardest and it has existed since 00002.
--
-- The one thing it cannot see is a role that has RLS applied to it today and
-- could remove it tomorrow — an owner can always issue NO FORCE ROW LEVEL
-- SECURITY. Nothing in the database can prevent that; what stops it is that
-- migrations are the only thing connecting as this role, and they are reviewed.
--
-- The body of this check is duplicated in internal/store/provisioning_test.go,
-- which evaluates it as the superuser to prove it actually refuses. Keep the
-- two in step.
-- +goose StatementBegin
DO $$
DECLARE
    is_super  boolean;
    is_bypass boolean;
BEGIN
    IF pg_catalog.row_security_active('public.users') THEN
        RETURN;
    END IF;

    SELECT r.rolsuper, r.rolbypassrls
      INTO is_super, is_bypass
      FROM pg_catalog.pg_roles r
     WHERE r.rolname = CURRENT_USER;

    RAISE EXCEPTION
        'migrations are running as %, which row-level security is not enforced against', CURRENT_USER
        USING
            DETAIL = format(
                'rolsuper=%s, rolbypassrls=%s, public.users is owned by %s. Every policy this schema defines is decorative for this role, so a migration applied by it proves nothing about isolation.',
                is_super,
                is_bypass,
                (SELECT c.relowner::regrole::text FROM pg_catalog.pg_class c WHERE c.oid = 'public.users'::regclass)
            ),
            HINT = 'Provision a dedicated non-superuser owner and point POSTGRES_MIGRATION_USER at it: apps/api/scripts/provision/bootstrap-owner.sql. See docs/adr/0005-database-role-provisioning.md.';
END
$$;
-- +goose StatementEnd

-- # Part two: hold the memberships without inheriting them
--
-- 00004 and 00005 each do `GRANT collabboard_identity TO CURRENT_USER`, because
-- ALTER FUNCTION ... OWNER requires the caller to be able to SET ROLE to the new
-- owner. A plain GRANT carries SET, which is what those migrations need — and
-- also INHERIT, which they do not.
--
-- INHERIT is not free here. The pre-tenant policies added by 00004 are attached
-- `TO collabboard_identity` with `USING (true)`, and PostgreSQL applies a
-- policy's role list to any role that has the *privileges of* the named role.
-- An inheriting migration role therefore matched those policies, and a plain
-- `SELECT * FROM users` as the migration role returned the entire global user
-- directory — every address of every tenant — with no tenant context set and no
-- SET ROLE issued. Measured, not theorised: before this migration the owner saw
-- every row of users, organizations and memberships, and zero rows of projects,
-- boards, columns and cards.
--
-- That is not an escalation in the strict sense: this role owns those tables and
-- could issue NO FORCE ROW LEVEL SECURITY at any time. It is a blast radius. The
-- single most likely misconfiguration in this system is POSTGRES_USER being
-- pointed at the migration credentials, and the difference between that leaking
-- the user directory and leaking nothing is this one clause.
--
-- WITH INHERIT FALSE keeps SET, so everything 00004 and 00005 do still works —
-- ALTER FUNCTION ... OWNER succeeds, and an explicit SET ROLE is still available
-- to a human debugging the identity path. What goes away is the ambient version.
--
-- Placed here rather than edited into 00004 and 00005 so that a database already
-- migrated by an earlier version converges on the same state.
-- +goose StatementBegin
DO $$
DECLARE
    role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['collabboard_identity', 'collabboard_credentials']
    LOOP
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = role_name) THEN
            EXECUTE format('GRANT %I TO CURRENT_USER WITH INHERIT FALSE', role_name);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down

-- Part one has nothing to undo: it creates no object and changes no state, it
-- only refuses to proceed. A Down that removed the check would be a Down that
-- exists so the check can be skipped.
--
-- Part two does, and it is load-bearing. The Down sections of 00004 and 00005
-- run `DROP OWNED BY`, which PostgreSQL allows only to a role holding the
-- *privileges of* the role being dropped — SET is not enough. Restoring INHERIT
-- here is what keeps those rollbacks runnable as the owner rather than only as
-- a superuser.
-- +goose StatementBegin
DO $$
DECLARE
    role_name text;
BEGIN
    FOREACH role_name IN ARRAY ARRAY['collabboard_identity', 'collabboard_credentials']
    LOOP
        IF EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = role_name) THEN
            EXECUTE format('GRANT %I TO CURRENT_USER WITH INHERIT TRUE', role_name);
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd
