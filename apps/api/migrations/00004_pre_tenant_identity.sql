-- The pre-tenant identity path: the one explicit, narrow, audited way to touch
-- identity data before a tenant is known. See issue #13, ADR 0002, and the
-- header of 00002_tenancy.sql, which predicts this file.
--
-- # Why anything is needed at all
--
-- Three operations cannot run inside a tenant-scoped transaction, because at
-- the moment they run there is no tenant:
--
--   * login by email — you have an email, not an organization
--   * "which organizations do I belong to" — the answer spans tenants
--   * registering a user, including inviting one who already has an account in
--     some other organization — users.WITH CHECK requires a membership that
--     cannot exist before the user row does
--
-- collabboard_app is not a superuser, holds no BYPASSRLS and owns nothing, so
-- every policy applies to it. With app.tenant_id unset, current_tenant_id() is
-- NULL and users / memberships / organizations all return zero rows. That is
-- exactly the fail-closed behaviour ADR 0001 wants, and it is also why the app
-- role cannot do any of the three above by itself. Something has to be granted;
-- the only question is how narrowly.
--
-- # The mechanism
--
-- A second role, collabboard_identity, that:
--
--   * cannot log in (NOLOGIN). Nothing connects as it; it exists only to own
--     functions, so there is no credential to leak and no pool to point at it.
--   * is not a superuser and holds no BYPASSRLS, so row-level security applies
--     to it exactly as it applies to collabboard_app.
--   * holds column-level privileges on users, memberships and organizations
--     and *no privileges at all* on projects, boards, columns or cards. This is
--     the load-bearing part: even a future function body that selects from
--     cards fails with "permission denied for table cards" rather than
--     returning another tenant's data.
--
-- and four SECURITY DEFINER functions owned by that role, one per operation,
-- each returning only the columns its operation needs. collabboard_app holds
-- EXECUTE on those four functions and nothing else. So the pre-tenant surface
-- is not "the app role can read users without a tenant" — it is "the app role
-- can invoke these four named functions", which is a list a reviewer can read.
--
-- The policies added here are attached TO collabboard_identity. A policy with a
-- role list does not apply to any other role, so collabboard_app's view of
-- users, memberships and organizations is byte-for-byte what it was before this
-- migration, and the isolation matrix in internal/store/isolation_test.go is
-- unchanged.
--
-- What this path deliberately cannot do:
--
--   * read, write or even count anything in projects, boards, columns or cards
--   * enumerate the user directory — no function takes a pattern or returns a
--     set of users; the two lookups take one exact email and return at most one
--     row
--   * tell an admin anything about an organization they are not in — the invite
--     lookup returns a uuid and nothing else
--   * update or delete anything: the only write granted is INSERT on users

-- +goose Up

-- Created without LOGIN and without a password. A role that cannot connect
-- cannot be the subject of a stolen credential, and the migration below is the
-- only thing that ever needs to act as it.
--
-- Guarded for the same reason 00001 guards collabboard_app: CREATE ROLE has no
-- IF NOT EXISTS, and in a deployed environment Terraform may have provisioned
-- the role already. The unconditional ALTER that follows is the load-bearing
-- half — it asserts the attributes this path's safety depends on however the
-- role came to exist.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_identity') THEN
        CREATE ROLE collabboard_identity;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER ROLE collabboard_identity
    NOLOGIN
    NOSUPERUSER
    NOBYPASSRLS
    NOCREATEDB
    NOCREATEROLE
    NOREPLICATION
    NOINHERIT;

GRANT USAGE ON SCHEMA public TO collabboard_identity;

-- Reassigning ownership of a function requires the migration role to be able to
-- SET ROLE to the new owner, so it grants itself membership. Unconditional
-- rather than guarded: a superuser already passes any pg_has_role check, and in
-- PostgreSQL 16 a CREATEROLE role that creates another role gets ADMIN OPTION
-- but *not* SET, so "is a member" is true while ALTER FUNCTION still fails with
-- `must be able to SET ROLE`. A plain GRANT carries SET and is a no-op when the
-- membership already exists, which covers every case without asking which one
-- this is.
--
-- The membership is *not* revoked afterwards, and that is a decision rather than
-- an oversight. Handing it back would look tidier and buy nothing: the migration
-- role owns every table in the schema, so it can already run
-- `ALTER TABLE users NO FORCE ROW LEVEL SECURITY` and read the whole directory.
-- Revoking would also break rollback — DROP ROLE needs ADMIN OPTION on the role
-- being dropped, which is exactly what the revoke would give away, leaving the
-- Down migration unable to run as anything but a superuser.
--
-- What matters is that the *serving* role cannot assume this one.
-- collabboard_app is granted EXECUTE on four functions and nothing else, and
-- pretenant_narrow_test.go asserts it is not a member.
GRANT collabboard_identity TO CURRENT_USER;

-- Column-level grants, not table-level. The columns listed are exactly the ones
-- the four functions below read or write; anything else — a password hash added
-- by #8, say — is unreachable through this path until someone deliberately adds
-- it to this list, which is a reviewable line of SQL rather than an oversight.
GRANT SELECT (id, email, display_name, created_at) ON TABLE users TO collabboard_identity;
GRANT INSERT (email, display_name) ON TABLE users TO collabboard_identity;
GRANT SELECT (tenant_id, user_id, role, created_at) ON TABLE memberships TO collabboard_identity;
GRANT SELECT (id, name, slug) ON TABLE organizations TO collabboard_identity;

-- Row-level security stays enabled and forced on all three tables. These
-- policies are permissive and carry a role list, so they add a second way in
-- *for collabboard_identity only*; PostgreSQL never evaluates them for any
-- other role, and the existing tenant-isolation policies are untouched.
--
-- USING (true) is the honest expression of what this path is: identity lookup
-- is global by definition. The narrowing is done by the grants above, by the
-- function bodies, and by which functions collabboard_app may execute — not by
-- a predicate here that would have nothing to key on.
CREATE POLICY users_pre_tenant_identity ON users
    FOR SELECT
    TO collabboard_identity
    USING (true);

-- Registration only. There is no UPDATE or DELETE policy and no UPDATE or
-- DELETE grant, so this path can create an identity and can never modify or
-- remove one.
CREATE POLICY users_pre_tenant_registration ON users
    FOR INSERT
    TO collabboard_identity
    WITH CHECK (true);

CREATE POLICY memberships_pre_tenant_identity ON memberships
    FOR SELECT
    TO collabboard_identity
    USING (true);

CREATE POLICY organizations_pre_tenant_identity ON organizations
    FOR SELECT
    TO collabboard_identity
    USING (true);

-- Every function below is SECURITY DEFINER with a fixed search_path. The fixed
-- search_path is not optional: without it, a caller could point search_path at
-- a schema of their own and have `users` resolve to a table they control, which
-- turns a definer function into arbitrary execution as its owner. pg_catalog
-- leads so that the operators and functions used in the bodies cannot be
-- shadowed either.
--
-- Every reference in every body is schema- and table-qualified, so nothing in a
-- body can bind to a function parameter by accident.

-- Login. Returns the columns an authentication handler needs to identify the
-- account and nothing else. Credential columns do not exist yet — #8 adds them
-- along with the code that checks them, and adding one means editing this
-- function and the grant above, which is the review point.
--
-- Case-insensitive to match users_email_key, which is UNIQUE on lower(email);
-- the index therefore serves this lookup and at most one row can come back.
-- +goose StatementBegin
CREATE FUNCTION public.identity_find_user_by_email(p_email text)
RETURNS TABLE (id uuid, email text, display_name text, created_at timestamptz)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT u.id, u.email, u.display_name, u.created_at
    FROM public.users u
    WHERE lower(u.email) = lower(btrim(p_email));
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.identity_find_user_by_email(text) IS
    'Pre-tenant identity path (issue #13): resolve one account by email for login. Runs as collabboard_identity, which can see no tenant-scoped table.';

-- Inviting someone who already has an account elsewhere. Deliberately a
-- different function from the one above rather than a reuse of it: the caller
-- here is an admin in some *other* organization, and the only thing they are
-- entitled to learn is whether the address they typed already maps to an
-- account, so that the membership can point at it. A uuid answers that and
-- discloses nothing about the account's name or its other organizations.
--
-- Set-returning rather than a scalar returning NULL, so "no such address" is no
-- row and not a zero uuid. uuid.Nil is a syntactically valid identifier that
-- matches nothing, and a caller that forgot to check would go on to create a
-- membership pointing at it — the same trap store.ErrNoTenant exists to avoid.
-- +goose StatementBegin
CREATE FUNCTION public.identity_resolve_user_id_by_email(p_email text)
RETURNS TABLE (user_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT u.id
    FROM public.users u
    WHERE lower(u.email) = lower(btrim(p_email));
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.identity_resolve_user_id_by_email(text) IS
    'Pre-tenant identity path (issue #13): map an email to a user id so an admin can invite an existing account. Returns the id and nothing else.';

-- The organization switcher. Cross-tenant by definition — the whole question is
-- "which tenants", so no tenant can be current while it is asked.
--
-- It takes a user id and does not authorize: the caller must pass the id of the
-- already-authenticated subject and nothing else. That is stated here because
-- the function cannot enforce it, and it is the one way this path can be
-- misused without touching any of the machinery above.
-- +goose StatementBegin
CREATE FUNCTION public.identity_list_user_organizations(p_user_id uuid)
RETURNS TABLE (organization_id uuid, name text, slug text, role text, joined_at timestamptz)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT o.id, o.name, o.slug, m.role, m.created_at
    FROM public.memberships m
    JOIN public.organizations o ON o.id = m.tenant_id
    WHERE m.user_id = p_user_id
    ORDER BY o.name;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.identity_list_user_organizations(uuid) IS
    'Pre-tenant identity path (issue #13): the organizations one user belongs to, across tenants. Does not authorize — callers must pass the authenticated subject.';

-- Registration, and the "invite someone with no account yet" half of invites. A
-- user row has to exist before any membership can reference it, and the
-- users WITH CHECK policy requires a membership to exist first, so this is the
-- one operation that is permanently impossible from a tenant-scoped
-- transaction rather than merely inconvenient there.
--
-- A duplicate email raises unique_violation (23505) from users_email_key rather
-- than returning the existing row: "this address is already registered" and
-- "here is the account you just made" are different answers and the caller has
-- to be able to tell them apart. The invite flow asks the resolve function
-- first precisely so it never has to.
-- +goose StatementBegin
CREATE FUNCTION public.identity_create_user(p_email text, p_display_name text)
RETURNS TABLE (id uuid, email text, display_name text, created_at timestamptz)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    INSERT INTO public.users (email, display_name)
    VALUES (btrim(p_email), btrim(p_display_name))
    RETURNING users.id, users.email, users.display_name, users.created_at;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.identity_create_user(text, text) IS
    'Pre-tenant identity path (issue #13): create a global identity. Impossible from a tenant-scoped transaction, because the membership that would satisfy the users policy cannot exist before the user does.';

-- A role can only be given ownership of an object in a schema it may create in,
-- so CREATE is granted for the length of the four statements below and revoked
-- immediately after. Ownership survives the revoke; the ability to add anything
-- new to the schema does not, which leaves the identity role with USAGE and its
-- four functions and nothing else.
--
-- Superusers skip this check, so without it the migration would work locally and
-- fail the first time it ran as the non-superuser owner issue #14 introduces.
GRANT CREATE ON SCHEMA public TO collabboard_identity;

-- Ownership, then privileges, in that order: ALTER FUNCTION ... OWNER rewrites
-- the grantor recorded in the ACL, so granting first and reassigning after
-- would leave grants attributed to a role that no longer owns the function.
ALTER FUNCTION public.identity_find_user_by_email(text) OWNER TO collabboard_identity;
ALTER FUNCTION public.identity_resolve_user_id_by_email(text) OWNER TO collabboard_identity;
ALTER FUNCTION public.identity_list_user_organizations(uuid) OWNER TO collabboard_identity;
ALTER FUNCTION public.identity_create_user(text, text) OWNER TO collabboard_identity;

REVOKE CREATE ON SCHEMA public FROM collabboard_identity;

-- PostgreSQL grants EXECUTE on new functions to PUBLIC by default, which for a
-- SECURITY DEFINER function means every role in the cluster can act as its
-- owner. Revoking that is the difference between a narrow door and an open one.
REVOKE ALL ON FUNCTION public.identity_find_user_by_email(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.identity_resolve_user_id_by_email(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.identity_list_user_organizations(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.identity_create_user(text, text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION public.identity_find_user_by_email(text) TO collabboard_app;
GRANT EXECUTE ON FUNCTION public.identity_resolve_user_id_by_email(text) TO collabboard_app;
GRANT EXECUTE ON FUNCTION public.identity_list_user_organizations(uuid) TO collabboard_app;
GRANT EXECUTE ON FUNCTION public.identity_create_user(text, text) TO collabboard_app;

-- +goose Down

DROP FUNCTION IF EXISTS public.identity_create_user(text, text);

DROP FUNCTION IF EXISTS public.identity_list_user_organizations(uuid);

DROP FUNCTION IF EXISTS public.identity_resolve_user_id_by_email(text);

DROP FUNCTION IF EXISTS public.identity_find_user_by_email(text);

DROP POLICY IF EXISTS organizations_pre_tenant_identity ON organizations;

DROP POLICY IF EXISTS memberships_pre_tenant_identity ON memberships;

DROP POLICY IF EXISTS users_pre_tenant_registration ON users;

DROP POLICY IF EXISTS users_pre_tenant_identity ON users;

-- DROP OWNED BY revokes the column-level grants and the schema usage, which is
-- what lets DROP ROLE succeed. The functions are already gone by this point, so
-- the role owns nothing.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_identity') THEN
        EXECUTE 'DROP OWNED BY collabboard_identity';
        EXECUTE 'DROP ROLE collabboard_identity';
    END IF;
END
$$;
-- +goose StatementEnd
