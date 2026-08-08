-- LOCAL DEVELOPMENT ONLY.
--
-- Migration 00001 creates the collabboard_app role without a password on
-- purpose: a password baked into a versioned migration can never be rotated,
-- and would be a committed credential in every environment that ran it. In
-- deployed environments the password is set from the secret store as part of
-- provisioning.
--
-- The compose stack has no secret store, so the local password is set here
-- instead. "dev" matches the credentials already in docker-compose.yml and
-- apps/api/.env.example — this file adds no secret that is not already public
-- in this repo, and the value is meaningless outside a laptop's Postgres
-- container.
--
-- Run once after `api migrate up`:
--
--   docker compose exec -T postgres psql -U collabboard -d collabboard \
--     < apps/api/scripts/dev/set-app-role-password.sql

ALTER ROLE collabboard_app WITH PASSWORD 'dev';
