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
docker compose up -d                 # Postgres + Redis

cd apps/api
go run ./cmd/api migrate up          # create the schema, RLS policies and app role

# One-time, local only: migration 00001 creates collabboard_app without a
# password, because a credential in a versioned migration can never be rotated.
# Deployed environments set it from the secret store; the laptop sets it here.
docker compose exec -T postgres psql -U collabboard -d collabboard \
  < scripts/dev/set-app-role-password.sql

go run ./cmd/api                     # serve on :8080
```

`api migrate` also takes `down` (one step), `reset` (all the way back) and
`status`. It connects as `POSTGRES_MIGRATION_USER` — the role that owns the
schema — while the server connects as `POSTGRES_USER`, which must be
`collabboard_app`. Those two are deliberately different roles: see
`docs/adr/0001-tenant-isolation.md`.

The web app is not yet initialised:

```bash
cd apps/web && npx create-next-app@latest . --typescript
```

## Commands

- API: `go build ./...` · `go test ./...` · `golangci-lint run`
- Web: `npm run dev` · `npm test` · `npm run lint`
- E2E: `npx playwright test` (needs both services, or the compose stack)
