-- Bound organizations.name, the one user-supplied name the schema did not.
--
-- See issue #67.
--
-- Every other name a caller can set is bounded by the application at 200 runes
-- (maxNameLength in internal/api/crud.go, and now maxOrganizationNameLength in
-- internal/auth). This column had only `CHECK (length(btrim(name)) > 0)` from
-- 00002, so the ceiling was whatever fit inside #50's 16 KiB body limit — which
-- is a containment, not a bound: an 8,000-character workspace name was accepted,
-- stored, turned into a slug, and rendered into every member's UI.
--
-- # Why the constraint as well as the check in Go
--
-- The application check is what produces a 400 with a sentence naming the field;
-- a constraint violation would surface as a 500. So the Go check is the answer
-- and this is the backstop, exactly as `length(btrim(name)) > 0` is the backstop
-- for a blank name today.
--
-- It is worth having because the column has more than one writer and is about to
-- have another: Register and CreateFirstOrganization both reach it through
-- provisionOrganization, and #90 adds a rename that will not. A constraint makes
-- the bound true of the *data*; a check in one function makes it true of one
-- code path.
--
-- # Why the two counts differ, and why that is the safe direction
--
-- Postgres `length()` counts characters — code points, in a UTF-8 database — so
-- it agrees with Go's utf8.RuneCountInString. `btrim` is where they part: the Go
-- check counts the value as stored, and this counts it with surrounding
-- whitespace removed. So Go is the stricter of the two and this can only ever
-- fire on a value Go did not see, which is what a backstop is for.
--
-- A new migration rather than an edit to 00002, for the reason forward
-- migrations exist: 00002 has already been applied everywhere it is going to be,
-- and goose will not run it again.

-- +goose Up

ALTER TABLE organizations
    ADD CONSTRAINT organizations_name_length
    CHECK (length(btrim(name)) <= 200);

COMMENT ON CONSTRAINT organizations_name_length ON organizations IS
    'Issue #67. The bound is maxOrganizationNameLength in internal/auth; the application check is what produces the 400, this makes it true of the data.';

-- +goose Down

ALTER TABLE organizations
    DROP CONSTRAINT organizations_name_length;
