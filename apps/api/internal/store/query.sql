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
-- id is the tiebreaker, not decoration. Positions are unique within a board by
-- construction (see MoveColumn), but "by construction" is an argument about the
-- write path, and a list whose order depends on that argument holding would
-- return rows in an unspecified order the day it stops. See ADR 0004.
SELECT *
FROM columns
WHERE board_id = @board_id
ORDER BY position, id;

-- name: ListCardsByBoard :many
-- The board view loads every card for the board in one round trip and groups by
-- column client-side; cards_tenant_board_idx serves this.
SELECT *
FROM cards
WHERE board_id = @board_id
ORDER BY column_id, position, id;

-- name: CreateOrganization :one
-- Registration creates the tenant it is about to belong to.
--
-- The id is public.current_tenant_id() rather than a parameter, which reads
-- oddly until you notice it is the same rule as every other write in this file:
-- the tenant comes from the transaction, never from the caller. An organization
-- *is* its tenant, so its primary key is that value. The caller generates a
-- uuid, opens WithTenant against it, and inserts — which is exactly the
-- sequence 00002_tenancy.sql predicts when it says creating an organization
-- "works under this policy without an exception".
--
-- The WITH CHECK half of organizations_tenant_isolation is therefore
-- unreachable in practice here: there is no argument to get wrong.
INSERT INTO organizations (id, name, slug)
VALUES (public.current_tenant_id(), @name, @slug)
RETURNING *;

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

-- name: GetMembership :one
-- The row joining one user to the *current* tenant, or no row at all.
--
-- Added for the WebSocket hub (issue #9), which has to answer "is this subject
-- still a member of this organization?" repeatedly over a connection that
-- outlives the request that opened it. ListMembers would answer it too, at the
-- cost of transferring every colleague to check one; this reads one row from
-- memberships_tenant_id_user_id_key.
--
-- No tenant_id parameter, per the convention in this file: the policy supplies
-- it. The (tenant_id, user_id) unique key means at most one row can come back,
-- so :one is safe rather than optimistic. A user who was never a member and a
-- user whose membership was revoked are the same answer — no row — which is the
-- answer the caller wants for both.
SELECT *
FROM memberships
WHERE user_id = @user_id;

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

-- name: GetUser :one
-- One user row, read under the current tenant's context (issue #75).
--
-- Same table and same policy as ListMembers, one row instead of all of them:
-- users has no tenant_id, and users_visible_via_membership makes a row visible
-- only when a membership joins it to the current tenant. So an id naming
-- somebody outside this organization returns *no row* rather than their
-- address, and this query can no more enumerate the global directory than
-- ListMembers can — it is a narrowing of ListMembers, not a new reach.
--
-- It takes a user id because there is no "current user" GUC to derive one from;
-- app.tenant_id is the only context a transaction carries. What stops it being
-- steered is above it, in exactly the way principal.TenantID is: the one caller
-- (auth.Service.Me, for GET /me) passes the id from the verified access token,
-- and no route, header, query parameter or body field in this service carries a
-- user id at all. See internal/api/auth_bola_test.go.
--
-- The column list is explicit rather than SELECT *: this reads a global
-- identity, and a future column on `users` should have to be named here before
-- it can travel to a client.
SELECT
    u.id,
    u.email,
    u.display_name
FROM users u
WHERE u.id = @user_id;

-- name: GetProject :one
SELECT *
FROM projects
WHERE id = @project_id;

-- name: UpdateProject :one
-- A PATCH: sqlc.narg makes "absent" a distinguishable value, so omitting a field
-- leaves the column alone instead of blanking it. The alternative — read, merge
-- in Go, write back — would be a lost update between the two statements.
UPDATE projects
SET name        = coalesce(sqlc.narg('name'), name),
    description = coalesce(sqlc.narg('description'), description)
WHERE id = @project_id
RETURNING *;

-- name: ArchiveProject :one
-- Idempotent: archiving an archived project keeps the original timestamp and
-- still returns the row, so a retried request is a success rather than a 404.
UPDATE projects
SET archived_at = coalesce(archived_at, now())
WHERE id = @project_id
RETURNING *;

-- name: CreateBoard :one
-- INSERT ... SELECT rather than VALUES, so that a project id naming a row this
-- tenant cannot see produces *no row* instead of a foreign-key violation. The
-- policy filters the SELECT, the caller gets ErrNoRows, and the handler answers
-- 404 — the same answer as an id that exists nowhere, which is what stops the
-- endpoint from being an existence oracle for other tenants.
INSERT INTO boards (tenant_id, project_id, name)
SELECT public.current_tenant_id(), p.id, @name
FROM projects p
WHERE p.id = @project_id
RETURNING *;

-- name: ListBoardsByProject :many
SELECT *
FROM boards
WHERE project_id = @project_id
ORDER BY name, id;

-- name: LockBoard :one
-- Serialises column reordering within one board.
--
-- FOR UPDATE on the *parent* rather than on the sibling rows being reordered is
-- deliberate: it is one lock per move, so two moves can never hold one lock each
-- and wait for the other's. Locking the siblings instead would deadlock as soon
-- as two moves crossed between the same pair of parents.
SELECT *
FROM boards
WHERE id = @board_id
FOR UPDATE;

-- name: UpdateBoard :one
UPDATE boards
SET name = @name
WHERE id = @board_id
RETURNING *;

-- name: DeleteBoard :execrows
-- Cascades to columns and cards through the composite foreign keys. :execrows
-- rather than :exec because "0 rows" is the only way this can report that the id
-- named no board the caller can see.
DELETE FROM boards
WHERE id = @board_id;

-- name: CreateColumn :one
-- Appends: one past the current maximum. Run under LockBoard, so two concurrent
-- creates cannot read the same maximum and both claim it.
INSERT INTO columns (tenant_id, board_id, name, position)
SELECT public.current_tenant_id(), b.id, @name,
       coalesce((SELECT max(c.position) FROM columns c WHERE c.board_id = b.id), 0) + 1
FROM boards b
WHERE b.id = @board_id
RETURNING *;

-- name: GetColumn :one
SELECT *
FROM columns
WHERE id = @column_id;

-- name: LockColumn :one
-- Serialises card creation and card moves *into* this column. See LockBoard for
-- why the lock is taken on the parent row.
SELECT *
FROM columns
WHERE id = @column_id
FOR UPDATE;

-- name: UpdateColumn :one
UPDATE columns
SET name = @name
WHERE id = @column_id
RETURNING *;

-- name: MoveColumn :one
-- Reorder: place this column immediately after @after_column_id, or first when
-- that argument is null.
--
-- "After this neighbour" rather than "at index 4" is the point. An index is a
-- claim about a list the client last saw, and two clients holding the same stale
-- list produce two writes that disagree about what index 4 means. A neighbour is
-- a claim about one row, which the database can still evaluate after someone
-- else has changed the list.
WITH anchor AS (
    -- Empty when @after_column_id is null (move to the front), and *also* empty
    -- when it names a column that is not a sibling — including one in another
    -- tenant, which the policy has already made invisible. The WHERE clause
    -- below tells those two cases apart.
    SELECT position
    FROM columns
    WHERE id = sqlc.narg('after_column_id')::uuid
      AND board_id = @board_id
      AND id <> @column_id
),
gap AS (
    SELECT
        (SELECT position FROM anchor) AS lower_bound,
        (SELECT min(position)
         FROM columns
         WHERE board_id = @board_id
           AND id <> @column_id
           AND (NOT EXISTS (SELECT 1 FROM anchor)
                OR position > (SELECT position FROM anchor))) AS upper_bound
)
UPDATE columns
SET position = (
        SELECT CASE
                   WHEN g.lower_bound IS NULL AND g.upper_bound IS NULL THEN 1
                   WHEN g.lower_bound IS NULL THEN g.upper_bound - 1
                   WHEN g.upper_bound IS NULL THEN g.lower_bound + 1
                   -- Exact. See the header comment: * 0.5, never / 2.
                   ELSE (g.lower_bound + g.upper_bound) * 0.5
               END
        FROM gap g
    )
WHERE columns.id = @column_id
  AND columns.board_id = @board_id
  AND (sqlc.narg('after_column_id')::uuid IS NULL OR EXISTS (SELECT 1 FROM anchor))
RETURNING *, scale(columns.position) > 100 AS needs_rebalance;

-- name: RebalanceBoardColumns :exec
-- Renumbers a board's columns to 1..n, collapsing accumulated fractional scale
-- without changing the order. Called only when MoveColumn reports pressure, and
-- only while LockBoard is held, so no concurrent move can be computing a
-- midpoint against a position this is about to rewrite.
UPDATE columns c
SET position = ranked.rank
FROM (
    SELECT sib.id, row_number() OVER (ORDER BY sib.position, sib.id) AS rank
    FROM columns sib
    WHERE sib.board_id = @board_id
) ranked
WHERE c.id = ranked.id;

-- name: DeleteColumn :one
-- RETURNING rather than a row count, because the deleted row is the only place
-- the column's board id still exists once the statement has run — and #45 needs
-- it to address the event that tells every other client the column is gone.
-- "No row" and "not this tenant's" remain the same answer, which is what the
-- 404 in internal/api/columns.go is built on.
DELETE FROM columns
WHERE id = @column_id
RETURNING *;

-- name: CreateCard :one
-- Appends to the end of the column, taking board_id from the column rather than
-- from the caller: the composite foreign key would reject a disagreement, but
-- deriving it means there is no argument to disagree with. Run under LockColumn.
--
-- assignee_id and due_at are plain nullable arguments here, unlike in UpdateCard
-- below: on an INSERT there is no prior value, so "absent" and "null" mean the
-- same thing and one parameter says both.
--
-- The composite foreign key (tenant_id, assignee_id) -> memberships means the
-- database refuses an assignee who is not a member of this tenant. That is the
-- backstop, not the check: a violation would surface as an unmapped error, so
-- the handler asks GetMembership first and answers 400. See internal/api/cards.go.
INSERT INTO cards (tenant_id, board_id, column_id, title, description, position,
                   assignee_id, due_at)
SELECT public.current_tenant_id(), c.board_id, c.id, @title, @description,
       coalesce((SELECT max(x.position) FROM cards x WHERE x.column_id = c.id), 0) + 1,
       sqlc.narg('assignee_id'), sqlc.narg('due_at')
FROM columns c
WHERE c.id = @column_id
RETURNING *;

-- name: GetCard :one
SELECT *
FROM cards
WHERE id = @card_id;

-- name: ListCardsByColumn :many
SELECT *
FROM cards
WHERE column_id = @column_id
ORDER BY position, id;

-- name: UpdateCard :one
-- Two different shapes in one statement, because the columns differ in kind.
--
-- title and description are NOT NULL, so `coalesce(narg, col)` says everything
-- there is to say: a null argument means "leave it alone", and there is no
-- third state to express.
--
-- assignee_id and due_at ARE nullable, and for them coalesce is not enough --
-- it cannot tell "leave it alone" from "set it to null", because both arrive as
-- a null argument. Clearing an assignee is a thing a user does, so the caller
-- has to be able to say it. Hence the paired boolean: set_assignee = false
-- leaves the column untouched, set_assignee = true writes whatever assignee_id
-- holds, null included.
--
-- The alternative would be a sentinel value meaning "clear", which is the same
-- ambiguity moved somewhere it is harder to see.
UPDATE cards
SET title       = coalesce(sqlc.narg('title'), title),
    description = coalesce(sqlc.narg('description'), description),
    assignee_id = CASE WHEN @set_assignee::bool THEN sqlc.narg('assignee_id') ELSE assignee_id END,
    due_at      = CASE WHEN @set_due_at::bool   THEN sqlc.narg('due_at')      ELSE due_at      END
WHERE id = @card_id
RETURNING *;

-- name: MoveCard :one
-- The headline operation: put this card in @column_id, immediately after
-- @after_card_id, or first in that column when @after_card_id is null.
--
-- One row is written. Neither the source column nor the destination is
-- renumbered, so a concurrent move of a *different* card is unaffected, and a
-- concurrent move of the *same* card is resolved by the row lock on the card
-- itself: the second transaction's UPDATE waits, then re-evaluates the CASE
-- against the order the first one left behind. The result is one of the two
-- requested orders, never a blend of both and never two cards sharing a rank.
--
-- Two things this refuses, both by returning no row rather than by raising:
--
--   * an @after_card_id that is not currently a card in @column_id — including
--     one from another tenant, which the policy has already hidden. Silently
--     treating it as "move to the front" would turn a stale client into a
--     wrong-but-successful write.
--   * a @column_id on a different board from the card. Cards do not change
--     board: the composite foreign key would reject it anyway, and #45 fans
--     events out per board, so a move that changed board would have to announce
--     itself in two rooms at once.
--
-- The midpoint is (lower + upper) * 0.5 and never (lower + upper) / 2. Postgres
-- numeric multiplication is exact — the result's scale is the sum of the
-- operands' — while division picks a result scale of at most max(the inputs'
-- scales, ~16 significant digits) and *rounds* to it. Under division, halving
-- stops making progress after roughly 53 nested inserts into one gap and starts
-- returning a value equal to one of the bounds, which is two cards with the same
-- rank. Under multiplication the midpoint is always strictly between them.
-- See docs/adr/0004-card-ordering.md.
WITH anchor AS (
    SELECT position
    FROM cards
    WHERE id = sqlc.narg('after_card_id')::uuid
      AND column_id = @column_id
      AND id <> @card_id
),
gap AS (
    SELECT
        (SELECT position FROM anchor) AS lower_bound,
        (SELECT min(position)
         FROM cards
         WHERE column_id = @column_id
           AND id <> @card_id
           AND (NOT EXISTS (SELECT 1 FROM anchor)
                OR position > (SELECT position FROM anchor))) AS upper_bound
)
UPDATE cards
SET column_id = @column_id,
    position = (
        SELECT CASE
                   WHEN g.lower_bound IS NULL AND g.upper_bound IS NULL THEN 1
                   WHEN g.lower_bound IS NULL THEN g.upper_bound - 1
                   WHEN g.upper_bound IS NULL THEN g.lower_bound + 1
                   ELSE (g.lower_bound + g.upper_bound) * 0.5
               END
        FROM gap g
    )
WHERE cards.id = @card_id
  AND EXISTS (
      SELECT 1
      FROM columns tgt
      WHERE tgt.id = @column_id
        AND tgt.board_id = cards.board_id
  )
  AND (sqlc.narg('after_card_id')::uuid IS NULL OR EXISTS (SELECT 1 FROM anchor))
RETURNING *, scale(cards.position) > 100 AS needs_rebalance;

-- name: RebalanceColumnCards :exec
-- The cost fractional ranking is chosen with, made explicit: nesting a move into
-- the same gap n times leaves a rank with n decimal places, and numeric tops out
-- at 16383 of them. Renumbering to 1..n collapses that, preserves the order, and
-- is the only statement here that writes every row in a column.
UPDATE cards c
SET position = ranked.rank
FROM (
    SELECT sib.id, row_number() OVER (ORDER BY sib.position, sib.id) AS rank
    FROM cards sib
    WHERE sib.column_id = @column_id
) ranked
WHERE c.id = ranked.id;

-- name: DeleteCard :one
-- RETURNING for the same reason as DeleteColumn: after this statement the card's
-- board and column ids exist nowhere else, and both are needed to tell the
-- board's other clients which card left which column.
DELETE FROM cards
WHERE id = @card_id
RETURNING *;
