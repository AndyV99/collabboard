# CollabBoard

Full-stack multi-tenant SaaS Kanban tool with real-time collaboration and Stripe billing. Global conventions and cross-cutting standards live in `~/.claude/CLAUDE.md` and the vault — this file only covers repo-local mechanics.

**Vault note**: `Projects/01 Full-Stack SaaS Platform.md` (path relative to the vault root — see the `Source of truth` section in the global `CLAUDE.md` for how to find it).

## Stack

- `apps/web` — Next.js (TypeScript), React Server Components + a WebSocket client for live updates.
- `apps/api` — Go (Gin) REST API + WebSocket hub. Module path: `github.com/AndyV99/collabboard/apps/api`.
- `infra/terraform` — VPC, ECS Fargate, RDS, ElastiCache, S3, ALB.
- `docs/adr` — architecture decision records.

## Decided stack — do not re-litigate these

These were chosen deliberately; the vault note leaves some of them open on
purpose, and this is where they got settled. Don't swap them out mid-project
without a new ADR.

| Concern | Choice | Why |
|---|---|---|
| HTTP framework | **Gin** | `net/http` compatible, so the WebSocket upgrade and `otelhttp` instrumentation work without adapters. Fiber's fasthttp would fight both. |
| DB access | **sqlc + pgx** | Hand-written SQL, generated type-safe Go. No hidden queries that could silently bypass tenant RLS. |
| Migrations | **goose** | Plain up/down SQL, embedded via `go:embed` so migrations ship with the binary. RLS policies expressed directly in SQL. |
| Job queue | **Asynq (Redis)** | Redis is already in the compose stack, so the dev loop works offline with no AWS credentials. |

The vault has been updated to match these choices — `Projects/01 Full-Stack
SaaS Platform.md` now names Asynq in both the Async work section and the
architecture diagram, so the vault and this file agree. Keep them that way: if
one of these decisions changes, change it in both places in the same session.

## Layout conventions (Go side)

Standard Go project layout: `cmd/api/main.go` as the entrypoint, business logic under `internal/`, with `internal/api` for HTTP handlers, `internal/realtime` for the WebSocket hub and Redis pub/sub fan-out, `internal/store` for the Postgres data layer. No business logic in `cmd/`.

## Local dev

`docker-compose.yml` at the repo root brings up Postgres + Redis. `apps/web` talks to `apps/api` on `localhost:8080` in dev.

Run `scripts/setup-hooks.sh` once per clone. It points `core.hooksPath` at `.githooks/`, which installs a pre-commit gitleaks scan. Git cannot install this itself and nothing warns you if you skip it — see the README's "Secret scanning" section for what still covers you if you do.

## Commands

- API: `go build ./...` · `go test ./...` · `golangci-lint run`
- Web: `npm run dev` · `npm test` · `npm run lint`
- E2E: `npx playwright test` (requires both services running, or the compose stack)
- Secrets: `scripts/gitleaks.sh git --no-banner .` — same binary and same `.gitleaks.toml` the hook and CI use

## Adding a test fixture that looks like a credential

`.gitleaks.toml`'s allowlist is scoped to values, never to paths, and should stay that way: excluding `**/*_test.go` would turn the scanner off in the place a real value is most likely to be pasted temporarily. If a new fixture trips a rule, add the literal value with a note on why it cannot be a real credential, or put a `gitleaks:allow` comment on the line for a one-off.

## Repo-specific notes

- Tenant isolation is enforced via Postgres row-level security — any new query against tenant-scoped tables must go through the existing `internal/store` helpers that set the tenant context, never a raw query that bypasses it.
- Stripe webhook handler must verify signatures and be idempotent (dedupe on Stripe's event ID) — this is exactly the kind of thing `review-standards` should specifically check on any PR touching billing.
