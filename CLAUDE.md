# CollabBoard

Full-stack multi-tenant SaaS Kanban tool with real-time collaboration and Stripe billing. Global conventions and cross-cutting standards live in `~/.claude/CLAUDE.md` and the vault — this file only covers repo-local mechanics.

**Vault note**: `Projects/01 Full-Stack SaaS Platform.md` (path relative to the vault root — see the `Source of truth` section in the global `CLAUDE.md` for how to find it).

## Stack

- `apps/web` — Next.js (TypeScript), React Server Components + a WebSocket client for live updates.
- `apps/api` — Go (Gin or Fiber) REST API + WebSocket hub.
- `infra/terraform` — VPC, ECS Fargate, RDS, ElastiCache, S3, ALB.
- `docs/adr` — architecture decision records.

## Layout conventions (Go side)

Standard Go project layout: `cmd/api/main.go` as the entrypoint, business logic under `internal/`, with `internal/api` for HTTP handlers, `internal/realtime` for the WebSocket hub and Redis pub/sub fan-out, `internal/store` for the Postgres data layer. No business logic in `cmd/`.

## Local dev

`docker-compose.yml` at the repo root brings up Postgres + Redis. `apps/web` talks to `apps/api` on `localhost:8080` in dev.

## Commands

- API: `go build ./...` · `go test ./...` · `golangci-lint run`
- Web: `npm run dev` · `npm test` · `npm run lint`
- E2E: `npx playwright test` (requires both services running, or the compose stack)

## Repo-specific notes

- Tenant isolation is enforced via Postgres row-level security — any new query against tenant-scoped tables must go through the existing `internal/store` helpers that set the tenant context, never a raw query that bypasses it.
- Stripe webhook handler must verify signatures and be idempotent (dedupe on Stripe's event ID) — this is exactly the kind of thing `review-standards` should specifically check on any PR touching billing.
