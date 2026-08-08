-- Tenancy: organizations (the tenant itself), users (global identities), and
-- memberships (the many-to-many that lets one person belong to several orgs).
--
-- Modelling note — why `users` has no tenant_id.
--
-- A person can belong to more than one organization, so identity cannot be
-- owned by one tenant: a tenant_id on `users` would mean one row, one password
-- and one email-uniqueness scope *per organization*, i.e. the same human as N
-- unrelated accounts. `users` is therefore global, and membership of an
-- organization is expressed by `memberships`, which is tenant-scoped.
--
-- Global does not mean readable. `users` still has RLS enabled and forced; its
-- policy is derived rather than direct — a row is visible to the current tenant
-- only if a membership joins it to that tenant. So the tenant-scoped connection
-- can read the colleagues it needs for assignees and member lists, and can
-- never enumerate the global user directory.
--
-- The consequence is deliberate and worth stating: identity lifecycle
-- (registration, login by email, "which orgs do I belong to") happens *before*
-- a tenant is known and cannot run through a tenant-scoped transaction. That
-- work needs the explicit, narrow, audited path the ADR describes for
-- cross-tenant access, not a widened policy here.

-- +goose Up

-- An organization *is* a tenant, so it has no separate tenant_id: its primary
-- key is the value that every other table carries as tenant_id, and its policy
-- keys on that. Adding a self-referential tenant_id column would be a second
-- name for the same value and a second thing to keep in sync.
CREATE TABLE organizations (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL CHECK (length(btrim(name)) > 0),
    slug       text NOT NULL CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,62}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE organizations IS
    'A tenant. organizations.id is the value carried as tenant_id by every tenant-scoped table.';

-- Slugs are globally unique because they appear in URLs. Uniqueness is enforced
-- by an index, which is not subject to RLS, so a conflicting insert reveals
-- that some other tenant holds the slug. That is the same information any
-- "workspace URL taken" check exposes and is accepted deliberately.
CREATE UNIQUE INDEX organizations_slug_key ON organizations (slug);

-- Global identity. Deliberately minimal: credential columns (password hash,
-- OAuth subject, email verification) belong to the auth work in issue #8, which
-- will add them alongside the code that uses them.
CREATE TABLE users (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email        text NOT NULL CHECK (position('@' IN email) > 1),
    display_name text NOT NULL CHECK (length(btrim(display_name)) > 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE users IS
    'Global identities, not tenant-scoped: one human, one row, however many organizations they belong to. Visibility is constrained by a membership-derived RLS policy.';

CREATE UNIQUE INDEX users_email_key ON users (lower(email));

CREATE TABLE memberships (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role       text NOT NULL DEFAULT 'member' CHECK (role IN ('owner', 'admin', 'member')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- Also the FK target for cards.assignee_id, which is what stops a card from
    -- being assigned to someone outside its organization.
    UNIQUE (tenant_id, user_id)
);

COMMENT ON TABLE memberships IS
    'Join between a global user and a tenant, carrying that user role within the tenant.';

-- "Which organizations does this user belong to" — the reverse of the
-- (tenant_id, user_id) unique index, which only serves lookups leading on
-- tenant_id.
CREATE INDEX memberships_user_id_idx ON memberships (user_id);

CREATE TRIGGER organizations_set_updated_at
    BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER memberships_set_updated_at
    BEFORE UPDATE ON memberships
    FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

-- Row-level security. FORCE is what makes this real: without it the table owner
-- (the role these migrations run as) is exempt, and every policy below would be
-- decorative for anyone connecting as the owner.
ALTER TABLE organizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE organizations FORCE ROW LEVEL SECURITY;

ALTER TABLE users ENABLE ROW LEVEL SECURITY;
ALTER TABLE users FORCE ROW LEVEL SECURITY;

ALTER TABLE memberships ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships FORCE ROW LEVEL SECURITY;

-- Creating an organization works under this policy without an exception:
-- the caller generates the uuid, sets app.tenant_id to it, and inserts.
CREATE POLICY organizations_tenant_isolation ON organizations
    FOR ALL
    USING (id = public.current_tenant_id())
    WITH CHECK (id = public.current_tenant_id());

-- Derived rather than direct: visible iff the current tenant has a membership
-- for the row. The WITH CHECK half means a tenant-scoped transaction cannot
-- create a user out of thin air, which is intended — see the header comment.
CREATE POLICY users_visible_via_membership ON users
    FOR ALL
    USING (
        EXISTS (
            SELECT 1
            FROM memberships m
            WHERE m.user_id = users.id
              AND m.tenant_id = public.current_tenant_id()
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1
            FROM memberships m
            WHERE m.user_id = users.id
              AND m.tenant_id = public.current_tenant_id()
        )
    );

CREATE POLICY memberships_tenant_isolation ON memberships
    FOR ALL
    USING (tenant_id = public.current_tenant_id())
    WITH CHECK (tenant_id = public.current_tenant_id());

-- Table-level grants only. The app role owns nothing, so DDL is impossible for
-- it regardless of what a compromised query tries.
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE organizations, users, memberships
    TO collabboard_app;

-- +goose Down

-- Dropped explicitly and first: a policy that reads another table depends on
-- it, so users_visible_via_membership makes `DROP TABLE memberships` fail while
-- it exists. The reverse order does not help either — memberships holds a
-- foreign key to users.
DROP POLICY IF EXISTS users_visible_via_membership ON users;

DROP TABLE IF EXISTS memberships;

DROP TABLE IF EXISTS users;

DROP TABLE IF EXISTS organizations;
