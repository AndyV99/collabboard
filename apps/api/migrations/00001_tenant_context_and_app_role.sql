-- Foundation for every migration that follows: the tenant-context accessor the
-- RLS policies are written against, the updated_at trigger, and the dedicated
-- application role.
--
-- See docs/adr/0001-tenant-isolation.md. Migrations run as the table owner;
-- the API connects as collabboard_app, which is not a superuser, does not hold
-- BYPASSRLS, and owns nothing.

-- +goose Up

-- current_tenant_id() is the single place the tenant GUC is read, so the
-- fail-closed behaviour is defined once rather than repeated in every policy.
--
-- The `true` argument to current_setting makes an unset GUC return NULL instead
-- of raising, and NULL propagates through `tenant_id = current_tenant_id()` as
-- NULL, which a policy treats as false. A transaction that forgets its
-- `SET LOCAL app.tenant_id` therefore sees zero rows rather than every row —
-- the property the ADR relies on.
-- +goose StatementBegin
CREATE FUNCTION public.current_tenant_id()
RETURNS uuid
LANGUAGE sql
STABLE
PARALLEL SAFE
AS $$
    SELECT nullif(current_setting('app.tenant_id', true), '')::uuid;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.current_tenant_id() IS
    'Tenant for the current transaction, from the app.tenant_id GUC set via SET LOCAL. NULL when unset, which makes every RLS policy fail closed.';

-- +goose StatementBegin
CREATE FUNCTION public.set_updated_at()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    NEW.updated_at := now();

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- The application role. Created without a password: credential lifecycle
-- belongs to the secret store, not to a versioned migration that can never
-- rotate it. `api provision` sets it from POSTGRES_PASSWORD — see
-- docs/adr/0006-database-role-provisioning.md.
--
-- CREATE ROLE is not idempotent and has no IF NOT EXISTS, so it is guarded —
-- in a deployed environment the role may already have been provisioned out of
-- band by Terraform. The negative attributes are spelled out even though they
-- are already CREATE ROLE's defaults, because this statement is the only place
-- this migration can *choose* them: PostgreSQL lets any role create a role
-- without SUPERUSER, BYPASSRLS, CREATEDB or REPLICATION, but only a role that
-- already holds one of those may change it afterwards. Hence the split below.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_app') THEN
        CREATE ROLE collabboard_app
            NOSUPERUSER
            NOBYPASSRLS
            NOCREATEDB
            NOCREATEROLE
            NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd

-- Asserted unconditionally, because the guarded CREATE above runs only when the
-- role is absent and a role provisioned by Terraform has to end up the same
-- shape as one created here.
--
-- Only the two attributes a non-superuser schema owner is actually permitted to
-- change appear here. PostgreSQL 16 rejects ALTER ROLE ... NOSUPERUSER,
-- NOBYPASSRLS, NOCREATEDB and NOREPLICATION unless the *caller* holds the
-- attribute being changed — asserting a negative counts as changing it — so
-- listing them here made the whole chain unrunnable as the owner role issue #14
-- introduces. They are checked instead, immediately below.
ALTER ROLE collabboard_app
    LOGIN
    NOCREATEROLE;

-- The attributes that cannot be set are verified instead, and a violation stops
-- the deploy. This is deliberately not equivalent to the ALTER it replaces: an
-- ALTER would quietly repair a role someone had provisioned with BYPASSRLS,
-- which reads as safety but means the mistake is never seen. A migration that
-- fails with the offending attribute named is the outcome worth having, and it
-- is the only one available to a role that cannot clear them anyway.
-- +goose StatementBegin
DO $$
DECLARE
    offending text;
BEGIN
    SELECT string_agg(a.attribute, ', ' ORDER BY a.attribute)
      INTO offending
      FROM pg_roles r
      CROSS JOIN LATERAL (
          VALUES ('SUPERUSER', r.rolsuper),
                 ('BYPASSRLS', r.rolbypassrls),
                 ('CREATEDB', r.rolcreatedb),
                 ('REPLICATION', r.rolreplication)
      ) AS a(attribute, is_held)
     WHERE r.rolname = 'collabboard_app'
       AND a.is_held;

    IF offending IS NOT NULL THEN
        RAISE EXCEPTION
            'collabboard_app holds %, which makes every row-level security policy in this schema decorative',
            offending
            USING HINT = 'Provision collabboard_app without those attributes. Migrations run as a non-superuser owner, which PostgreSQL does not allow to clear them. See apps/api/scripts/provision/bootstrap-owner.sql and docs/adr/0006-database-role-provisioning.md.';
    END IF;
END
$$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO collabboard_app;

-- +goose Down

-- DROP OWNED BY also revokes privileges granted to the role in this database,
-- which is what actually lets DROP ROLE succeed; the role owns no objects by
-- construction.
--
-- It needs the *privileges of* collabboard_app, not merely administrative
-- rights over it. PostgreSQL 16 gives a CREATEROLE role that creates another
-- role ADMIN OPTION with INHERIT FALSE and SET FALSE, so DROP ROLE succeeds
-- while DROP OWNED BY fails with "Only roles with privileges of role
-- collabboard_app may drop objects owned by it" — and then DROP ROLE fails too,
-- because the privileges DROP OWNED BY would have revoked still depend on it.
-- A plain GRANT carries INHERIT and is a no-op when the membership already
-- exists, which covers the superuser case and the Terraform case without asking
-- which one this is. It is in Down only: nothing in Up needs it.
--
-- Granted rather than left to chance, and inside the same guard as the drops so
-- that a Down over a database where the role is already gone is still a no-op.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_app') THEN
        EXECUTE 'GRANT collabboard_app TO CURRENT_USER';
        EXECUTE 'DROP OWNED BY collabboard_app';
        EXECUTE 'DROP ROLE collabboard_app';
    END IF;
END
$$;
-- +goose StatementEnd

DROP FUNCTION IF EXISTS public.set_updated_at();

DROP FUNCTION IF EXISTS public.current_tenant_id();
