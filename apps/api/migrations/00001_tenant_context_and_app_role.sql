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
-- belongs to the secret store (Secrets Manager in deployed environments,
-- apps/api/scripts/dev/set-app-role-password.sql locally), not to a versioned
-- migration that can never rotate it.
--
-- CREATE ROLE is not idempotent and has no IF NOT EXISTS, so it is guarded —
-- in a deployed environment the role may already have been provisioned out of
-- band by Terraform. The unconditional ALTER that follows is the load-bearing
-- part: it asserts the attributes this schema's isolation depends on, whether
-- the role was created here or elsewhere.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_app') THEN
        CREATE ROLE collabboard_app;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER ROLE collabboard_app
    LOGIN
    NOSUPERUSER
    NOBYPASSRLS
    NOCREATEDB
    NOCREATEROLE
    NOREPLICATION;

GRANT USAGE ON SCHEMA public TO collabboard_app;

-- +goose Down

-- DROP OWNED BY also revokes privileges granted to the role in this database,
-- which is what actually lets DROP ROLE succeed; the role owns no objects by
-- construction.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_app') THEN
        EXECUTE 'DROP OWNED BY collabboard_app';
        EXECUTE 'DROP ROLE collabboard_app';
    END IF;
END
$$;
-- +goose StatementEnd

DROP FUNCTION IF EXISTS public.set_updated_at();

DROP FUNCTION IF EXISTS public.current_tenant_id();
