-- name: ListProjects :many
-- Active projects in the current tenant.
--
-- No tenant_id parameter, here or anywhere else in this file: the RLS policy
-- supplies `tenant_id = current_tenant_id()`, and the planner can still use
-- projects_tenant_id_idx for it because current_tenant_id() is STABLE. Repeating
-- the predicate by hand would buy nothing and would invite a caller to pass a
-- tenant that disagrees with the transaction's. See internal/store/README.md.
SELECT *
FROM projects
WHERE archived_at IS NULL
ORDER BY name;

-- name: CreateProject :one
-- The tenant comes from the transaction, never from the caller: an INSERT cannot
-- name a tenant it is not already scoped to, so the WITH CHECK half of the
-- policy is unreachable in practice rather than merely unviolated.
INSERT INTO projects (tenant_id, name, description)
VALUES (public.current_tenant_id(), @name, @description)
RETURNING *;

-- name: GetBoard :one
-- Returns no row rather than another tenant's board when the id belongs to
-- someone else, which is the behaviour the isolation model is chosen for.
SELECT *
FROM boards
WHERE id = @board_id;

-- name: ListColumnsByBoard :many
SELECT *
FROM columns
WHERE board_id = @board_id
ORDER BY position;

-- name: ListCardsByBoard :many
-- The board view loads every card for the board in one round trip and groups by
-- column client-side; cards_tenant_board_idx serves this.
SELECT *
FROM cards
WHERE board_id = @board_id
ORDER BY column_id, position;

-- name: CreateMembership :one
-- The second half of an invite, and the half that is *not* pre-tenant.
--
-- Once the pre-tenant path has resolved (or created) the global user, joining
-- them to an organization is ordinary tenant-scoped work: the tenant comes from
-- the transaction, and an admin scoped to their own organization cannot add a
-- member to anyone else's. Adding the membership is also what makes the user
-- visible to this tenant at all, since users_visible_via_membership is derived
-- from this table.
INSERT INTO memberships (tenant_id, user_id, role)
VALUES (public.current_tenant_id(), @user_id, @role)
RETURNING *;

-- name: ListMembers :many
-- Joins the tenant-scoped memberships table to the global users table. users has
-- no tenant_id: its policy makes a row visible only when a membership joins it
-- to the current tenant, so this returns colleagues and can never enumerate the
-- user directory.
SELECT
    m.id AS membership_id,
    m.user_id,
    m.role,
    m.created_at AS joined_at,
    u.email,
    u.display_name
FROM memberships m
JOIN users u ON u.id = m.user_id
ORDER BY u.display_name;
