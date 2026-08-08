-- Domain: projects -> boards -> columns -> cards.
--
-- Two things are load-bearing beyond the obvious columns.
--
-- 1. tenant_id is denormalised onto every level rather than reached by joining
--    up the hierarchy. RLS policies have to be evaluated per row on the table
--    being read, so the discriminator has to be *on* that table; a join would
--    also mean the policy could not use an index.
--
-- 2. Child rows reference their parent by (parent_id, tenant_id) against a
--    composite unique key, not by parent_id alone. That makes it structurally
--    impossible for a board in tenant A to hang off a project in tenant B —
--    a class of bug RLS does not catch, because each row looks fine in
--    isolation and only the relationship between them is wrong.

-- +goose Up

CREATE TABLE projects (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES organizations (id) ON DELETE CASCADE,
    name        text NOT NULL CHECK (length(btrim(name)) > 0),
    description text NOT NULL DEFAULT '',
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- Redundant as a uniqueness claim (id is already the PK); it exists so
    -- boards can point at (project_id, tenant_id).
    UNIQUE (id, tenant_id)
);

-- The unique index above leads on id, so it cannot serve "projects in this
-- tenant". This one can.
CREATE INDEX projects_tenant_id_idx ON projects (tenant_id);

CREATE TABLE boards (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL,
    project_id  uuid NOT NULL,
    name        text NOT NULL CHECK (length(btrim(name)) > 0),
    archived_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    UNIQUE (id, tenant_id),

    FOREIGN KEY (project_id, tenant_id)
        REFERENCES projects (id, tenant_id) ON DELETE CASCADE
);

-- Boards by project, the list query behind a project page.
CREATE INDEX boards_tenant_project_idx ON boards (tenant_id, project_id);

CREATE TABLE columns (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL,
    board_id   uuid NOT NULL,
    name       text NOT NULL CHECK (length(btrim(name)) > 0),

    -- numeric, not integer: drag-and-drop reordering inserts *between* two
    -- neighbours, and an exact-precision type lets that be a single-row update
    -- (midpoint of the two) instead of renumbering the whole list under a
    -- concurrent edit. numeric rather than double precision so repeated
    -- halving stays exact.
    position   numeric NOT NULL,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),

    -- FK target for cards: carrying board_id into the key is what proves a
    -- card's column and board agree with each other.
    UNIQUE (id, board_id, tenant_id),

    FOREIGN KEY (board_id, tenant_id)
        REFERENCES boards (id, tenant_id) ON DELETE CASCADE
);

CREATE INDEX columns_tenant_board_position_idx ON columns (tenant_id, board_id, position);

CREATE TABLE cards (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL,
    board_id    uuid NOT NULL,
    column_id   uuid NOT NULL,
    title       text NOT NULL CHECK (length(btrim(title)) > 0),
    description text NOT NULL DEFAULT '',
    position    numeric NOT NULL,
    assignee_id uuid,
    due_at      timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- One FK covers board, column and tenant together. A separate
    -- (board_id, tenant_id) -> boards FK would be redundant: this key already
    -- pins the card to a column that belongs to exactly this board, and
    -- columns is in turn pinned to a board in exactly this tenant.
    FOREIGN KEY (column_id, board_id, tenant_id)
        REFERENCES columns (id, board_id, tenant_id) ON DELETE CASCADE,

    -- The assignee must be a member of *this* organization, so the reference is
    -- to memberships rather than to users. MATCH SIMPLE means the check is
    -- skipped entirely while assignee_id is NULL, which is what allows an
    -- unassigned card. The column list on SET NULL (PostgreSQL 15+) nulls only
    -- the assignee when a membership is revoked; the plain form would try to
    -- null tenant_id too and violate its NOT NULL.
    FOREIGN KEY (tenant_id, assignee_id)
        REFERENCES memberships (tenant_id, user_id) ON DELETE SET NULL (assignee_id)
);

-- Cards by board: the board view loads every card in one query and orders
-- within column client-side, so this is the hot path.
CREATE INDEX cards_tenant_board_idx ON cards (tenant_id, board_id);

-- Cards within a column, in order: the narrower read behind moving a card.
CREATE INDEX cards_tenant_column_position_idx ON cards (tenant_id, column_id, position);

-- "My cards" across a tenant. Partial, because unassigned is the common state
-- and those rows are never the answer to this query.
CREATE INDEX cards_tenant_assignee_idx ON cards (tenant_id, assignee_id)
    WHERE assignee_id IS NOT NULL;

CREATE TRIGGER projects_set_updated_at
    BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER boards_set_updated_at
    BEFORE UPDATE ON boards
    FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER columns_set_updated_at
    BEFORE UPDATE ON columns
    FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

CREATE TRIGGER cards_set_updated_at
    BEFORE UPDATE ON cards
    FOR EACH ROW EXECUTE FUNCTION public.set_updated_at();

ALTER TABLE projects ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects FORCE ROW LEVEL SECURITY;

ALTER TABLE boards ENABLE ROW LEVEL SECURITY;
ALTER TABLE boards FORCE ROW LEVEL SECURITY;

ALTER TABLE columns ENABLE ROW LEVEL SECURITY;
ALTER TABLE columns FORCE ROW LEVEL SECURITY;

ALTER TABLE cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE cards FORCE ROW LEVEL SECURITY;

-- USING governs which rows are visible and updatable; WITH CHECK governs the
-- row after the write. Both are needed: without WITH CHECK a tenant could
-- update a row's tenant_id and push it into someone else's org.
CREATE POLICY projects_tenant_isolation ON projects
    FOR ALL
    USING (tenant_id = public.current_tenant_id())
    WITH CHECK (tenant_id = public.current_tenant_id());

CREATE POLICY boards_tenant_isolation ON boards
    FOR ALL
    USING (tenant_id = public.current_tenant_id())
    WITH CHECK (tenant_id = public.current_tenant_id());

CREATE POLICY columns_tenant_isolation ON columns
    FOR ALL
    USING (tenant_id = public.current_tenant_id())
    WITH CHECK (tenant_id = public.current_tenant_id());

CREATE POLICY cards_tenant_isolation ON cards
    FOR ALL
    USING (tenant_id = public.current_tenant_id())
    WITH CHECK (tenant_id = public.current_tenant_id());

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE projects, boards, columns, cards
    TO collabboard_app;

-- +goose Down

DROP TABLE IF EXISTS cards;

DROP TABLE IF EXISTS columns;

DROP TABLE IF EXISTS boards;

DROP TABLE IF EXISTS projects;
