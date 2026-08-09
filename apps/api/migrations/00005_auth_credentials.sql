-- Password credentials: a separate schema, a separate role, and a verifier that
-- is not the thing the password hashes to. See issue #8 and
-- docs/adr/0003-password-verifier-storage.md.
--
-- # The problem this migration is careful about
--
-- Migration 00004 built a deliberately narrow pre-tenant door: collabboard_app
-- may execute four SECURITY DEFINER functions owned by collabboard_identity,
-- and collabboard_identity holds *column-level* SELECT on
-- users (id, email, display_name, created_at) and nothing else. Its own header
-- says so explicitly:
--
--     "anything else — a password hash added by #8, say — is unreachable
--      through this path until someone deliberately adds it to this list"
--
-- Auth needs somewhere to put a password, and login has to check it before any
-- tenant exists, so the credential has to be reachable from the pre-tenant
-- path. The lazy version of this migration is one line — grant
-- collabboard_identity SELECT on a new users.password_hash column — and it
-- would quietly make every existing identity function, and every future one, a
-- potential hash-disclosure endpoint. This migration does not do that.
--
-- # What it does instead
--
-- Three separations, each of which stands alone:
--
--   1. **A different schema.** Credentials live in `auth`, not `public`.
--      collabboard_app holds no USAGE on `auth`, so the serving role cannot
--      name the table at all — not to select from it, not to describe it. This
--      is a stronger boundary than RLS: RLS filters rows from a table you can
--      see, schema USAGE decides whether the table exists as far as you are
--      concerned.
--
--   2. **A different role.** collabboard_credentials owns the three functions
--      below and holds column privileges on exactly one table in exactly one
--      schema. It has no privilege of any kind on users, memberships,
--      organizations, or any tenant-scoped table — so it is strictly narrower
--      than collabboard_identity, not a widening of it. Symmetrically,
--      collabboard_identity gets nothing in `auth`: the identity door did not
--      widen by one column.
--
--   3. **A stored value that is not the password hash.** The column is called
--      `verifier`, not `password_hash`, because it is sha256() of the argon2id
--      output rather than the output itself. The application computes
--      argon2id — memory-hard KDF work belongs on the tier that scales
--      horizontally, not on the database — and sends the derived key; the
--      database hashes it once more and compares. Two consequences:
--
--        * nothing any function returns is crackable material. The params
--          function hands back a salt and cost parameters, which are public by
--          construction, and the verify function hands back a uuid or no row.
--        * a stolen dump is not pass-the-hash-able, because `verifier` is a
--          one-way image of the value the verify function expects.
--
--      This is the shape SCRAM (RFC 5802) uses for exactly this reason, and the
--      shape PostgreSQL itself stores its own scram-sha-256 passwords in:
--      StoredKey = H(ClientKey). Nothing here invents a primitive — argon2id in
--      the application, sha256 in the database, both standard.
--
-- # What is deliberately absent
--
--   * No UPDATE and no DELETE, at grant level or policy level. The one write is
--      an INSERT with no ON CONFLICT clause, so this path can create a
--      credential once and can never change or remove one. Password change and
--      password reset are separate features that will need their own function,
--      their own grant and their own review.
--   * No lookup by email. Every function takes a user id, which is why
--      collabboard_credentials needs no privilege on `users` whatsoever. The
--      caller resolves the email through the identity door first.
--   * No pgcrypto. sha256() is built into pg_catalog (PostgreSQL 11+), so there
--      is no extension to install and no chance of the hash function being
--      shadowed by search_path.
--
-- # What the caller still has to get right
--
-- identity_create_password takes a user id and does not authorize, exactly as
-- identity_list_user_organizations does not. The handler must pass the id of
-- the user it just created or the already-authenticated subject. The function
-- cannot enforce that; it is stated here because it is the one way this path
-- can be misused without touching any of the machinery above.

-- +goose Up

-- A schema whose only job is to be one the serving role cannot enter.
--
-- No GRANT to collabboard_app appears anywhere below, and PostgreSQL grants
-- nothing on a new schema to PUBLIC, so the absence is the mechanism. The
-- REVOKE is belt and braces against a future default changing under us.
CREATE SCHEMA auth;

REVOKE ALL ON SCHEMA auth FROM PUBLIC;

COMMENT ON SCHEMA auth IS
    'Credential storage. collabboard_app holds no USAGE here by design: only collabboard_credentials, through the SECURITY DEFINER functions in public. See issue #8 and ADR 0003.';

-- Created without LOGIN and without a password, for the same reason
-- collabboard_identity is: a role that cannot connect cannot be the subject of
-- a stolen credential, and nothing but this migration ever needs to act as it.
--
-- Guarded because CREATE ROLE has no IF NOT EXISTS and Terraform may have
-- provisioned it (issue #14). The negative attributes are stated on the CREATE
-- because that is the only statement here that may choose them; the ALTER and
-- the assertion below split for the reason 00001 explains.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_credentials') THEN
        CREATE ROLE collabboard_credentials
            NOSUPERUSER
            NOBYPASSRLS
            NOCREATEDB
            NOCREATEROLE
            NOREPLICATION;
    END IF;
END
$$;
-- +goose StatementEnd

ALTER ROLE collabboard_credentials
    NOLOGIN
    NOCREATEROLE
    NOINHERIT;

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
     WHERE r.rolname = 'collabboard_credentials'
       AND a.is_held;

    IF offending IS NOT NULL THEN
        RAISE EXCEPTION
            'collabboard_credentials holds %, so the credential functions it owns would not be bounded by the schema and policy boundaries this migration builds',
            offending
            USING HINT = 'Provision collabboard_credentials without those attributes. See apps/api/scripts/provision/bootstrap-owner.sql and docs/adr/0006-database-role-provisioning.md.';
    END IF;
END
$$;
-- +goose StatementEnd

-- Reassigning ownership of a function requires the migration role to be able to
-- SET ROLE to the new owner. Same reasoning, and same deliberate non-revoke, as
-- 00004: the migration role owns every table already, so handing the membership
-- back would buy nothing while breaking rollback (DROP ROLE needs the ADMIN
-- OPTION the revoke would give away). What matters is that collabboard_app
-- cannot assume this role, which credentials_test.go asserts directly.
GRANT collabboard_credentials TO CURRENT_USER;

GRANT USAGE ON SCHEMA auth TO collabboard_credentials;

-- One row per user who has a password. Absent for a user who has none — an
-- account created by an invite that has not been accepted, or (later) one that
-- authenticates only through an external provider. Login treats "no row" and
-- "wrong password" identically; see internal/auth.
--
-- The columns other than `verifier` are the KDF's public parameters. They are
-- not secrets: argon2id's security rests on the cost of the derivation, not on
-- the salt being hidden, and the application has to know them to reproduce the
-- derivation at all.
CREATE TABLE auth.user_credentials (
    user_id     uuid PRIMARY KEY REFERENCES public.users (id) ON DELETE CASCADE,

    -- Named rather than assumed, so a future migration that introduces a second
    -- KDF has somewhere to say which row uses which. The CHECK is what makes
    -- adding one a deliberate edit instead of a silent divergence.
    algorithm   text NOT NULL CHECK (algorithm = 'argon2id'),

    -- 16 bytes is the argon2 specification's recommended salt length and the
    -- floor RFC 9106 gives. Per row, from crypto/rand.
    salt        bytea NOT NULL CHECK (octet_length(salt) >= 16),

    memory_kib  integer NOT NULL CHECK (memory_kib >= 19456),
    iterations  integer NOT NULL CHECK (iterations >= 2),
    parallelism integer NOT NULL CHECK (parallelism BETWEEN 1 AND 255),
    key_length  integer NOT NULL CHECK (key_length >= 16),

    -- sha256() of the argon2id output, not the output itself. Exactly 32 bytes
    -- because that is what sha256 produces; a row of any other width means
    -- something wrote a value that did not come from the function below.
    verifier    bytea NOT NULL CHECK (octet_length(verifier) = 32),

    created_at  timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE auth.user_credentials IS
    'Password verifiers: sha256(argon2id(password)). Not the password hash — the stored value cannot be replayed to the verify function. See ADR 0003.';

COMMENT ON COLUMN auth.user_credentials.verifier IS
    'sha256() of the argon2id derived key. The application never receives this column; it sends the derived key and the database hashes and compares.';

-- There is no updated_at column and no set_updated_at trigger here, unlike
-- every table in `public`. A row of this table is immutable by construction —
-- no UPDATE grant, no UPDATE policy, no statement that would use either — so a
-- column tracking when it last changed would only ever record the insert.
-- Whichever migration adds password change adds all three together.

-- Row-level security on a table nobody but its definer functions can name is
-- belt and braces, and it is one line. FORCE matters for the usual reason: the
-- migration role owns this table, and without FORCE the policies below would
-- not apply to it.
ALTER TABLE auth.user_credentials ENABLE ROW LEVEL SECURITY;
ALTER TABLE auth.user_credentials FORCE ROW LEVEL SECURITY;

-- USING (true) is honest about what this is: credential lookup is by primary
-- key and there is no tenant to key on. The narrowing is done by the schema
-- boundary, the column grants and the three functions — not by a predicate
-- here that would have nothing to filter.
CREATE POLICY user_credentials_verify ON auth.user_credentials
    FOR SELECT
    TO collabboard_credentials
    USING (true);

-- Registration only. There is no UPDATE or DELETE policy and no UPDATE or
-- DELETE grant, so this path can create a credential and can never modify or
-- remove one.
CREATE POLICY user_credentials_register ON auth.user_credentials
    FOR INSERT
    TO collabboard_credentials
    WITH CHECK (true);

-- Column-level, not table-level, for the same reason 00004's grants are: the
-- columns listed are exactly the ones the three functions read or write, and a
-- column added later is unreachable until someone edits this list.
GRANT SELECT (user_id, salt, memory_kib, iterations, parallelism, key_length, verifier)
    ON TABLE auth.user_credentials TO collabboard_credentials;
GRANT INSERT (user_id, algorithm, salt, memory_kib, iterations, parallelism, key_length, verifier)
    ON TABLE auth.user_credentials TO collabboard_credentials;

-- The three functions live in `public` because that is where collabboard_app
-- can reach them; the data they touch lives in `auth`, where it cannot. Each is
-- SECURITY DEFINER with a pinned search_path — without it a caller could point
-- `user_credentials` at a table they control and have the function operate on
-- it as its owner — and every reference in every body is schema-qualified.

-- Step one of a login: the KDF parameters needed to reproduce the derivation.
--
-- Returns no secret. The salt and the cost parameters are public by
-- construction: the client of this function is the code that has to run
-- argon2id with them, and argon2id's strength is the cost of the derivation
-- rather than the secrecy of its inputs. `verifier` is not in the return list
-- and there is no function that returns it.
--
-- No row means the user has no password. The caller must not branch visibly on
-- that — internal/auth derives deterministic stand-in parameters and does the
-- full derivation anyway, so an absent account costs the same as a present one.
-- +goose StatementBegin
CREATE FUNCTION public.identity_password_params(p_user_id uuid)
RETURNS TABLE (salt bytea, memory_kib integer, iterations integer, parallelism integer, key_length integer)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT c.salt, c.memory_kib, c.iterations, c.parallelism, c.key_length
    FROM auth.user_credentials c
    WHERE c.user_id = p_user_id;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.identity_password_params(uuid) IS
    'Credential path (issue #8): the public KDF parameters for one user. Returns no secret and no verifier; no row means the user has no password.';

-- Step two of a login: the comparison, done inside the database.
--
-- p_key is the raw argon2id output, not a password and not the stored value.
-- The database hashes it once more and compares, which is what makes the stored
-- verifier useless to anyone holding a dump: replaying it here fails, because
-- what is compared is sha256(what you sent) and what is stored is
-- sha256(the real key).
--
-- Returns a uuid or no row. There is no boolean, no error message and no
-- distinction between "no such user", "no password set" and "wrong password" —
-- all three are the empty set, so a caller cannot accidentally leak which one
-- happened by forwarding a status.
--
-- The bytea comparison is not constant time; PostgreSQL's bytea equality is a
-- length check and a memcmp. That is accepted deliberately rather than
-- overlooked: extracting a byte of `verifier` through it would mean resolving a
-- few nanoseconds of memcmp through an HTTP request, a rate limiter, an ~80ms
-- argon2id derivation and a network round trip to Postgres, and the value
-- recovered would still not be a password. ADR 0003 records the reasoning.
-- +goose StatementBegin
CREATE FUNCTION public.identity_verify_password(p_user_id uuid, p_key bytea)
RETURNS TABLE (user_id uuid)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT c.user_id
    FROM auth.user_credentials c
    WHERE c.user_id = p_user_id
      AND c.verifier = pg_catalog.sha256(p_key);
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.identity_verify_password(uuid, bytea) IS
    'Credential path (issue #8): compare a derived key against the stored verifier inside the database. Returns a user id or no row — never a reason.';

-- Registration. INSERT with no ON CONFLICT, so a second call for a user who
-- already has a credential raises unique_violation (23505) rather than
-- overwriting one. That is the whole of "this path cannot change a password":
-- there is no UPDATE grant, no UPDATE policy, and no statement that would use
-- them. A password-change or reset feature has to add its own function, which
-- is the review point.
--
-- The application sends the derived key; the stored verifier is sha256 of it,
-- computed here. The application never computes or sees the stored value.
-- +goose StatementBegin
CREATE FUNCTION public.identity_create_password(
    p_user_id     uuid,
    p_salt        bytea,
    p_memory_kib  integer,
    p_iterations  integer,
    p_parallelism integer,
    p_key_length  integer,
    p_key         bytea
)
RETURNS TABLE (user_id uuid)
LANGUAGE sql
VOLATILE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    INSERT INTO auth.user_credentials
        (user_id, algorithm, salt, memory_kib, iterations, parallelism, key_length, verifier)
    VALUES
        (p_user_id, 'argon2id', p_salt, p_memory_kib, p_iterations, p_parallelism, p_key_length,
         pg_catalog.sha256(p_key))
    RETURNING user_credentials.user_id;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION public.identity_create_password(uuid, bytea, integer, integer, integer, integer, bytea) IS
    'Credential path (issue #8): store a verifier for a user who has none. Cannot overwrite — no UPDATE grant, no UPDATE policy, no ON CONFLICT clause.';

-- Ownership, then privileges, in that order: ALTER FUNCTION ... OWNER rewrites
-- the grantor recorded in the ACL, so granting first and reassigning after
-- would leave grants attributed to a role that no longer owns the function.
--
-- CREATE on public is granted for exactly these three statements and revoked
-- immediately: ownership survives the revoke, the ability to add anything else
-- to the schema does not. Superusers skip this check, so without it the
-- migration would work locally and fail the first time it ran as the
-- non-superuser owner issue #14 introduces.
GRANT CREATE ON SCHEMA public TO collabboard_credentials;

ALTER FUNCTION public.identity_password_params(uuid) OWNER TO collabboard_credentials;
ALTER FUNCTION public.identity_verify_password(uuid, bytea) OWNER TO collabboard_credentials;
ALTER FUNCTION public.identity_create_password(uuid, bytea, integer, integer, integer, integer, bytea) OWNER TO collabboard_credentials;

REVOKE CREATE ON SCHEMA public FROM collabboard_credentials;

-- PostgreSQL grants EXECUTE on a new function to PUBLIC by default, which for a
-- SECURITY DEFINER function means every role in the cluster can act as its
-- owner. Revoking that is the difference between a narrow door and an open one.
REVOKE ALL ON FUNCTION public.identity_password_params(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.identity_verify_password(uuid, bytea) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.identity_create_password(uuid, bytea, integer, integer, integer, integer, bytea) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION public.identity_password_params(uuid) TO collabboard_app;
GRANT EXECUTE ON FUNCTION public.identity_verify_password(uuid, bytea) TO collabboard_app;
GRANT EXECUTE ON FUNCTION public.identity_create_password(uuid, bytea, integer, integer, integer, integer, bytea) TO collabboard_app;

-- +goose Down

DROP FUNCTION IF EXISTS public.identity_create_password(uuid, bytea, integer, integer, integer, integer, bytea);

DROP FUNCTION IF EXISTS public.identity_verify_password(uuid, bytea);

DROP FUNCTION IF EXISTS public.identity_password_params(uuid);

DROP TABLE IF EXISTS auth.user_credentials;

DROP SCHEMA IF EXISTS auth;

-- DROP OWNED BY revokes the grants that would otherwise make DROP ROLE fail.
-- The functions and the schema are already gone, so the role owns nothing.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'collabboard_credentials') THEN
        EXECUTE 'DROP OWNED BY collabboard_credentials';
        EXECUTE 'DROP ROLE collabboard_credentials';
    END IF;
END
$$;
-- +goose StatementEnd
