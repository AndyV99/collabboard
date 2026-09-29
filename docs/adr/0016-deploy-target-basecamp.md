# 0016. Deploy to the shared Basecamp platform; keep the AWS stack as the reference shape

Date: 2026-09-29
Status: accepted

## Context

CollabBoard has never been deployed. `infra/terraform` describes a faithful AWS
staging environment: a three-tier VPC, a NAT gateway, an ALB, two Fargate
services, RDS and ElastiCache (ADRs 0012, 0014, 0015). It is covered by 39
`terraform test` cases and costs about **$106/month** to leave running. The
backlog put deploy last on purpose to keep that bill off the clock while the app
was still changing. The result is an app nobody outside this machine has ever
been able to use.

The portfolio this repo belongs to was re-planned on 2026-09-29 around one
constraint: every project stays permanently live on a personal budget. The
answer was a shared platform, Basecamp (`AndyV99/basecamp`): a single EC2
`t4g.medium` running k3s behind Cloudflare Tunnel, with one CloudNativePG
cluster and one Valkey shared by all tenants, for about $32/month in total.
Basecamp's host layer is kept swappable so the same cluster can run on a home
server.

Nothing in the application depends on ECS, RDS or ElastiCache specifically.
Tenant isolation is Postgres RLS (0001); the role split is Postgres roles
(0006); realtime delivery is Redis pub/sub plus client re-fetch (0005, 0010).
The AWS coupling lives entirely in `infra/terraform` and in how one-shot
database work reaches the database (0013).

## Decision

**CollabBoard deploys to Basecamp as its first tenant.** `infra/terraform`
stays in the repo, still tested in CI, as the reference production shape. It is
not applied continuously. One recorded apply → smoke → destroy run (#200) proves
it applies.

ADRs 0012, 0014 and 0015 are not superseded. They remain correct descriptions of
the AWS shape, and their status now says their scope is that stack. What moves:

| Concern | AWS shape (0012/0014/0015) | Basecamp |
|---|---|---|
| Ingress | ALB, API not internet-facing | Cloudflare Tunnel → Traefik; the API is still reachable only from the web tier |
| Postgres | RDS, `force_ssl`, master secret behind an IAM explicit Deny | CNPG database + roles; superuser secret confined to the operator namespace |
| Redis | ElastiCache, TLS required | Valkey, ACL user per tenant |
| One-shot DB work (0013) | ECS run-task before the service rolls | Helm `pre-upgrade` hook Job; a failed migration still blocks the rollout |
| Deploy authority (0015) | Pipeline rolls services, never runs `terraform apply` | Pipeline bumps an image tag in the Basecamp repo; Flux reconciles |
| Environments | Long-lived staging | One live environment + per-PR preview namespaces |

## Consequences

**Easier.** CollabBoard is live for about $0 marginal cost, so everything the
backlog deferred until "after deploy" (e2e against a real URL, observability
with real traffic, realtime latency measured over the internet) can happen now.
The repo also gains a concrete cost argument that stays on one cloud: the same
app in two shapes, $106/month versus about $0 marginal.

**Three guarantees have to be re-proven, not assumed.** Each has its own issue:

- **The idle-timeout invariant** (0014 validated the ALB timeout against the hub's
  25 s ping). Behind Cloudflare there are several idle limits with different
  semantics, none of them set by this repo: #196.
- **Master-credential isolation.** An IAM explicit Deny becomes Kubernetes RBAC
  plus an admission policy. That is a different guarantee and needs its own
  mutation-verified test: #197.
- **Transport encryption to the database.** RDS enforced TLS. In-cluster it is a
  choice between CNPG-issued certificates and relying on NetworkPolicy, and it
  gets written down when the tenant is deployed (#195).

**Given up.** Running on managed AWS data services was the strongest
operations evidence in this repo, and it becomes reference material. The
hardening issues written for that shape (#56, #105, #136–#140) need
re-auditing against the new target (#201), not silent closure. A shared
Postgres means CollabBoard shares a failure domain with every other tenant.

**Reversal** is cheap in one direction. The AWS stack is kept tested precisely
so that moving back is an apply and a DNS change, not a rewrite.
