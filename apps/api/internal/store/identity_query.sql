-- The pre-tenant identity queries, and the complete list of them.
--
-- These generate into a *different* package from query.sql
-- (internal/store/internal/identitygen rather than .../gen), so the two doors
-- hand out two interfaces with no methods in common. Code holding a
-- store.IdentityQuerier cannot reach ListProjects, because the type has no such
-- method: the narrowness is a compile error, not a convention.
--
-- Every query below is one call to one SECURITY DEFINER function from migration
-- 00004. None of them names a table, and none of them can: without a tenant the
-- app role sees zero rows in users, memberships and organizations, and the only
-- thing it holds EXECUTE on is those four functions. Adding a query here cannot
-- widen the path on its own — a new capability needs a new function, a new
-- grant and a migration, and that is the review point.
--
-- Admission rule, applied to each of the four: the operation must be
-- *impossible* through Store.WithTenant, not merely awkward there. Anything a
-- tenant-scoped transaction could do belongs in query.sql instead.
--
-- The casts and the column-definition lists are not decoration. sqlc reads
-- CREATE FUNCTION from the migrations but does not expand a RETURNS TABLE into
-- output columns, so `SELECT *` off one of these functions types as a single
-- opaque `interface{}`. Naming the columns in the FROM clause and casting each
-- one gives sqlc the types it cannot infer, and the result is a generated
-- struct per row instead of an `any` every caller has to assert.

-- name: FindUserByEmail :one
-- Login. Pre-tenant because the input is an email: nothing has claimed an
-- organization yet, so the RLS policy on users has nothing to key on.
--
-- No row means no account with that address. #8 owns what a handler does about
-- that; this shape neither forces nor forecloses a constant-time response, but
-- it does mean the handler has to decide deliberately rather than inherit one.
SELECT
    f.id::uuid AS id,
    f.email::text AS email,
    f.display_name::text AS display_name,
    f.created_at::timestamptz AS created_at
FROM public.identity_find_user_by_email(@email) AS f(id, email, display_name, created_at);

-- name: ResolveUserIDByEmail :one
-- Inviting a user who already has an account, possibly in an organization the
-- inviting admin cannot see. Pre-tenant because looking outside the current
-- tenant's visibility is the entire point.
--
-- No row means the address is unregistered, which is the caller's signal to
-- create the user instead. It returns the id and nothing else on purpose: the
-- admin typed the address, so existence is all they learn, and nothing about
-- the account's name or its other organizations is disclosed.
SELECT r.user_id::uuid AS user_id
FROM public.identity_resolve_user_id_by_email(@email) AS r(user_id);

-- name: ListUserOrganizations :many
-- The organization switcher, immediately after login. Pre-tenant by
-- definition — the question *is* "which tenants", so none can be current while
-- it is asked, and the answer legitimately spans them.
--
-- The function does not authorize. Callers must pass the id of the
-- already-authenticated subject; passing one taken from a request body would
-- turn this into a membership-disclosure endpoint.
SELECT
    o.organization_id::uuid AS organization_id,
    o.name::text AS name,
    o.slug::text AS slug,
    o.role::text AS role,
    o.joined_at::timestamptz AS joined_at
FROM public.identity_list_user_organizations(@user_id) AS o(organization_id, name, slug, role, joined_at);

-- name: CreateUser :one
-- Registration, and the "invited address has no account yet" branch of an
-- invite. Permanently impossible from a tenant-scoped transaction rather than
-- merely inconvenient there: the users WITH CHECK policy requires a membership
-- joining the row to the current tenant, and that membership cannot exist
-- before the user row it references does.
--
-- A duplicate address surfaces as a unique violation (SQLSTATE 23505) from
-- users_email_key rather than returning the existing row, because "already
-- registered" and "here is the account you just made" are different answers.
-- The invite flow asks ResolveUserIDByEmail first so it never has to ask this.
SELECT
    c.id::uuid AS id,
    c.email::text AS email,
    c.display_name::text AS display_name,
    c.created_at::timestamptz AS created_at
FROM public.identity_create_user(@email, @display_name) AS c(id, email, display_name, created_at);

-- The three queries below are the credential half of the pre-tenant path, added
-- by issue #8. They travel the same Go door as the four above —
-- Store.WithoutTenant — but their SECURITY DEFINER functions are owned by
-- collabboard_credentials rather than collabboard_identity, and that role's
-- privileges are strictly narrower: one table, in a schema (`auth`) the serving
-- role holds no USAGE on, and nothing at all in `public`.
--
-- So the identity door did not widen to accommodate a password. A second,
-- smaller door was cut next to it. See migration 00005 and ADR 0003.
--
-- Admission rule, same as above: each has to be *impossible* through
-- Store.WithTenant. All three are, for the reason login is — they run before
-- any organization has been claimed, and their subject is a global user rather
-- than a tenant-scoped row.

-- name: PasswordParams :one
-- Step one of a login: the argon2id parameters needed to reproduce the
-- derivation for this user.
--
-- Returns no secret. A salt and a cost are public by construction — the code
-- that needs them is the code that has to run the KDF — and the verifier is not
-- in the function's return list at all. There is no query anywhere that returns
-- it, which is the property ADR 0003 is about.
--
-- No row means the user has no password set. internal/auth must not branch
-- visibly on that: it derives stand-in parameters and does the full derivation
-- anyway, so an unknown account costs what a known one does.
SELECT
    p.salt::bytea AS salt,
    p.memory_kib::integer AS memory_kib,
    p.iterations::integer AS iterations,
    p.parallelism::integer AS parallelism,
    p.key_length::integer AS key_length
FROM public.identity_password_params(@user_id) AS p(salt, memory_kib, iterations, parallelism, key_length);

-- name: VerifyPassword :one
-- Step two of a login: the comparison, performed inside the database.
--
-- The argument is the raw argon2id output, not the password and not the stored
-- value. Postgres hashes it once more and compares against the stored verifier,
-- so what is stored can never be replayed here.
--
-- A uuid or no row. Never a boolean and never a reason: "no such user", "no
-- password set" and "wrong password" are the same empty result, so no caller
-- can leak which one happened by forwarding a status it was handed.
SELECT v.user_id::uuid AS user_id
FROM public.identity_verify_password(@user_id, @key) AS v(user_id);

-- name: CreatePassword :one
-- Registration. Pre-tenant for the same reason CreateUser is: it runs in the
-- same transaction, immediately after the user row it refers to, and before any
-- organization exists to scope it to.
--
-- INSERT with no ON CONFLICT, so calling it for a user who already has a
-- credential raises unique_violation (23505) rather than silently overwriting
-- one. Changing a password is a different feature and needs a different
-- function, a different grant and its own review — this path cannot do it.
SELECT c.user_id::uuid AS user_id
FROM public.identity_create_password(
    @user_id, @salt, @memory_kib, @iterations, @parallelism, @key_length, @key
) AS c(user_id);
