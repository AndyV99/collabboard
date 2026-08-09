# CollabBoard

A multi-tenant, real-time Kanban / project-tracking tool (Trello/Linear-shaped,
deliberately scoped down). The end-to-end project: frontend, API design, data
modeling, auth, billing, and deployment in one coherent product.

**Vault note:** `Projects/01 Full-Stack SaaS Platform.md` — that note is the
source of truth for scope and architecture, not this README.

**Status:** Planned — scaffolded, not yet implemented.
**Build order:** #1 (first). Target ~10–12 weeks to demoable.

## Architecture

- **Frontend** — Next.js (TypeScript), React Server Components for initial page
  loads, client-side WebSocket for live updates.
- **Backend** — Go REST API (Gin or Fiber) plus a WebSocket hub for real-time
  fan-out. Go over Node deliberately, so the project isn't TypeScript front to
  back, and because the concurrency model suits the hub.
- **Data** — PostgreSQL (RDS) as system of record with row-level security for
  tenant isolation; Redis (ElastiCache) for session cache and pub/sub backbone
  so the hub scales horizontally.

## Layout

```
apps/web/            Next.js app
apps/api/            Go service
  cmd/api/           entrypoint — no business logic here
  internal/api/      HTTP handlers
  internal/migrate/  goose runner behind `api migrate`
  internal/realtime/ WebSocket hub + Redis pub/sub fan-out
  internal/store/    Postgres data layer (tenant-context helpers live here)
  migrations/        goose SQL, embedded into the binary
infra/terraform/     VPC, ECS Fargate, RDS, ElastiCache, S3, ALB
docs/adr/            architecture decision records
```

## Getting started

```bash
docker compose up -d                 # Postgres + Redis, and collabboard_owner

cd apps/api
go run ./cmd/api migrate up          # schema, RLS policies and the app roles
go run ./cmd/api provision           # app-role password, from POSTGRES_PASSWORD
go run ./cmd/api                     # serve on :8080
```

`api migrate` also takes `down` (one step), `reset` (all the way back) and
`status`. It connects as `POSTGRES_MIGRATION_USER` — `collabboard_owner`, which
owns the schema — while the server connects as `POSTGRES_USER`, which must be
`collabboard_app`. Those two are deliberately different roles, and neither is
the cluster's superuser: see `docs/adr/0001-tenant-isolation.md` and
`docs/adr/0006-database-role-provisioning.md`. `api migrate` refuses to run as a
role row-level security is not enforced against, before it applies anything.

`api provision` sets the app role's password to `POSTGRES_PASSWORD`. Migration
00001 creates `collabboard_app` without one on purpose — a credential in a
versioned migration can never be rotated — so this is the step that supplies it,
from configuration rather than from a file in this repository. In a deployed
environment `POSTGRES_PASSWORD` comes from the secret store and the command runs
as part of the pre-deploy task, next to `api migrate up`.

### If your Postgres volume predates the owner role

`collabboard_owner` is created by an init hook, and the postgres image runs those
only on an empty data directory. A checkout from before that hook existed has a
volume without it, and `api migrate up` will say so. Either start over:

```bash
docker compose down -v && docker compose up -d
```

or adopt the existing volume in place, keeping the data:

```bash
docker compose exec -T postgres psql -v ON_ERROR_STOP=1 \
  -U collabboard -d collabboard \
  -v owner_password=dev -v previous_owner=collabboard \
  -f /opt/collabboard/provision/bootstrap-owner.sql
```

## Database access

Every tenant-scoped query goes through `internal/store.WithTenant`, which opens
a transaction, sets `app.tenant_id` for its duration, and hands the callback a
querier bound to it. Nothing else in the service can reach the database: the
generated queriers live in `internal/store/internal/gen`, which Go's
internal-package rule puts out of reach of every package outside
`internal/store`. See `apps/api/internal/store/README.md` for the why and the
enforcement.

Queries are hand-written in `apps/api/internal/store/query.sql` and compiled by
sqlc:

```bash
cd apps/api && sqlc generate         # after editing query.sql or a migration
```

The generated code is committed, so a build never needs sqlc installed.

## Tests

Two loops, split by build tag.

```bash
cd apps/api
go test ./...                    # unit: no Docker, a couple of seconds
go test -tags=integration ./...  # integration: real Postgres in a container
```

The integration suite brings up its own Postgres with Testcontainers on a random
port, provisions `collabboard_owner` by running the same init hook and the same
`bootstrap-owner.sql` the compose stack and a deploy run, applies the real
migrations *as that non-superuser owner*, and connects as `collabboard_app` for
everything under test. A suite that migrates as a superuser passes every
isolation assertion while proving nothing, which is how the gap issue #14 closed
went unnoticed for five migrations. It asserts all of that rather than trusting
it, in `apps/api/internal/store/identity_test.go` and
`apps/api/internal/store/provisioning_test.go`.

It needs a running Docker daemon and nothing else: no compose stack, no
pre-provisioned database, no environment variables. Containers are removed when
the run ends, whether it passed or failed — Testcontainers' reaper handles the
case where the process dies without unwinding.

The harness lives in `apps/api/internal/testsupport/pgtest`.

The web app is not yet initialised:

```bash
cd apps/web && npx create-next-app@latest . --typescript
```

## Commands

- API: `go build ./...` · `go test ./...` · `go test -tags=integration ./...` · `golangci-lint run` · `sqlc generate`
- Web: `npm run dev` · `npm test` · `npm run lint`
- E2E: `npx playwright test` (needs both services, or the compose stack)
