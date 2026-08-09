#!/bin/sh
#
# Compose-stack hook. The postgres image runs everything in
# /docker-entrypoint-initdb.d once, on an empty data directory, as the bootstrap
# superuser — which is the only identity that can create the schema owner.
#
# It is a shell script rather than a .sql file because the image feeds .sql
# files to psql with no -v arguments, and bootstrap-owner.sql needs a password.
# The SQL itself is not duplicated here: this runs the same file a deployed
# environment runs, so "works locally" and "works in AWS" are the same claim.
#
# Existing volumes do not re-run this. A checkout that predates issue #14 has a
# pgdata volume with no collabboard_owner in it, and `api migrate up` will stop
# at migration 00006 saying so. Either recreate the volume:
#
#   docker compose down -v && docker compose up -d
#
# or adopt the existing one in place, which keeps the data:
#
#   docker compose exec -T postgres psql -v ON_ERROR_STOP=1 \
#     -U collabboard -d collabboard \
#     -v owner_password=dev -v previous_owner=collabboard \
#     -f /opt/collabboard/provision/bootstrap-owner.sql
#
# COLLABBOARD_OWNER_PASSWORD defaults to "dev" to match docker-compose.yml and
# apps/api/.env.example. It is not a secret and could not be one: it is the
# password of a role that only exists inside a container on a laptop, and the
# whole point of this issue is that the deployed equivalent comes from Secrets
# Manager instead.

set -eu

psql \
    --set ON_ERROR_STOP=1 \
    --username "$POSTGRES_USER" \
    --dbname "$POSTGRES_DB" \
    --set owner_password="${COLLABBOARD_OWNER_PASSWORD:-dev}" \
    --file /opt/collabboard/provision/bootstrap-owner.sql
