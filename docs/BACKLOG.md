# Ordered backlog

Every open issue, checked against the code as it stands on `main` and ordered
toward one goal: **an app that is worth deploying, deployed once, and not
before.** Nothing here is applied to AWS yet, deliberately — staging costs about
$107/month running, and the app is still changing.

Audited 2026-08-29 at `b230b0d`.

## How to use this

Work top to bottom within a phase. **Within a phase, items with no "after" line
can run in parallel**; anything with one has to wait. Phases are ordered by
dependency, not by importance — Stripe is the headline feature and it is in
phase 4 because the three phases before it are what make it safe to build.

Each entry says what it is, why it sits where it does, and what it depends on.
The issue itself carries the detail; this file carries the order.

**Before starting anything here, re-read the issue.** Several were written
months ago against code that has since moved, and the "Audit notes" section at
the bottom records which ones no longer describe reality.

## The target

Usable means: a person can sign up, make a workspace, invite somebody, run a
board, and pay for it — and see who a card belongs to while they do. Deployable
means: an operator can stand the whole thing up from an empty AWS account by
following one document, and see what it is doing once it runs.

Two things were deliberately cut from the pre-deploy set:

- **OAuth2 social login (#32)** — email/password auth already works and is
  carefully built. OAuth adds a second credential story to the part of this
  codebase with the most delicate invariants, for impressiveness rather than
  usability. After the deploy.
- **Realtime fan-out above a board (#52, #53)** — board and project creation are
  not announced live. Visible only if two people are looking at the same project
  list at the same moment. After the deploy.

---

## Phase 1 — Finish making what exists correct

The rule set earlier: fix what exists before building more. These are the last
of it, ordered by how quickly somebody hits them.

| # | What | Why here |
|---|---|---|
| **#157** | Assignee and due date on the board | The API has both since #48 and the UI shows neither, so the feature is invisible. Highest user-visible gap in the product. |
| **#86** | An account cannot create a second organization | `CreateFirstOrganization` is the only path. A user with two workspaces is stuck, and "create a workspace" is a thing the product implies it can do. |
| **#80** | The API proxy flattens every success status to 200 | A 201 Created reaches the browser as 200. Contradicts the proxy's own documented contract; cheap to fix. |
| **#84** | `NewRouter` silently serves no `/api/v1` routes when auth deps are nil | A deployment footgun: a misconfigured process starts, answers `/healthz`, and serves nothing else. Worth closing before anything is deployed. |
| **#90** | An organization's name is permanent | No rename endpoint. Small, and the absence is conspicuous next to project and board rename. |
| **#87** | `maxLength` counts UTF-16 units | **Partially fixed** — see audit notes. Two project forms still pass raw code-point limits, so an emoji in a project name is truncated at half length. |
| **#78** | Drop the `GET /members` workaround | `/me` exists and the web has an endpoint helper for it. Internal cleanup; do it while the surrounding code is fresh. |

**#157 first.** It is the one a person notices in the first thirty seconds.

---

## Phase 2 — Make the running system legible

Nothing after this phase is debuggable without it, and none of it can be
retrofitted cheaply once there is traffic. This is also the phase that makes the
first deploy worth watching rather than worth hoping about.

| # | What | Why here |
|---|---|---|
| **#95** | Almost no log line carries `request_id` | The foundation. Do this one first — #96 and #97 are both "log the thing that currently logs nothing", and they want the correlation id to already exist. |
| **#96** | A cross-tenant probe leaves no trace | Every 404 from an object-id route logs nothing, so the one attack this architecture is built to resist is invisible. *After #95.* |
| **#97** | The three 409s from `writeAuthError` log nothing | A rejected registration or member add is untraceable. *After #95.* |
| **#12** | RED/USE metrics and a Prometheus `/metrics` endpoint | `modules/ecs` sets `container_insights = false` **specifically because** this was the plan, so deploying without it means no application metrics at all. The ALB listener rules already 404 `/metrics` publicly, so the route has somewhere to live. |
| **#44** | Hub metrics — connections, rooms, drops by close code | The realtime half of the same picture. *After #12.* |

---

## Phase 3 — A net under the next two phases

| # | What | Why here |
|---|---|---|
| **#17** | Playwright harness + first smoke test | **Before Stripe, not after.** Stripe is the largest remaining change and touches auth, a migration and a new unauthenticated route; having e2e in place means it is protected while it is built rather than tested afterwards. It also gives #155's unverified acceptance criterion — that a malformed URL renders the not-found page in a real browser — somewhere to land, and it becomes the post-deploy smoke check `cd.yml` already calls. |

---

## Phase 4 — Stripe

The headline feature, and until 2026-08-29 it had no issue at all. Filed as
three so they can be worked separately.

| # | What | Why here |
|---|---|---|
| **#160** | Subscription model, checkout, and the organization↔customer link | The state the webhooks move. Nothing to act on without it. |
| **#161** | Webhook endpoint: signature verification and idempotency | **The one that matters.** The vault asks for "webhook handling and idempotency"; `CLAUDE.md` singles it out for review. Read its "three properties" section before starting — the body-limit interaction and the ordering hazard are both easy to get wrong and hard to see. *After #160.* |
| **#162** | Billing screen | A subscription nobody can see is not a feature. *After #160 and #161.* |

---

## Phase 5 — Security hardening the deploy should not go without

These are all real and none of them is urgent while the app runs only on a
laptop. They become urgent the moment there is a public hostname.

| # | What | Note |
|---|---|---|
| **#73** | Registration is not rate limited, despite a comment saying it is | The comment in `auth.go` is the one the issue names. An unbudgeted address-existence oracle on a public URL. |
| **#67** | `organization_name` is the one unbounded user-supplied field on registration | Reachable by anyone who can register. |
| **#71** | No request body size limit on the web proxy or auth handlers | The API bounds its own; the web tier in front of it does not. |
| **#69** | Refresh-token rotation races across processes | **Blocks web autoscaling** — `web_desired_count` is fixed at 1 with a comment saying "while #69 is open". Needed before the web tier can scale. |
| **#91** | `/api/auth/login`'s promise that it logs no email is not tested | The promise is real; nothing holds it. |
| **#117** | A file named `.gitleaks.toml` is silently skipped, anywhere in the tree | The config documents the hazard and works around it for itself. The gap is general. |
| **#114** | The libpq keyword/value DSN form is not caught | The other shape of the leak the custom rule was written for. |
| **#116** | `.gitleaks.toml` has no regression test | A rule can be broken silently. Do it with #114 and #117. |

---

## Phase 6 — Deploy

Only start this when phases 1–5 are done and the app is worth showing. The
Terraform, the CD pipeline, the image build and the policy gates are all already
merged and unapplied.

| # | What | Note |
|---|---|---|
| **register a domain in Route 53** | Not an issue — an operator action | The one prerequisite Terraform cannot create for itself. `web_hostname`, `api_hostname` and `route53_zone_id` are still `example.com` placeholders in `terraform.tfvars`. |
| **#56** | Provision the schema owner and the app-role secret | **Re-scoped** — see audit notes. Most of it landed with #102; what remains is the rotation runbook and the role-lifecycle decision. |
| **#105** | The operator runbook | **No longer blocked.** Its dependencies (#101, #102, #103) are all merged. This is the document that makes the first apply possible, and it must be written against merged code — which it now can be. |
| **#144** | Nothing bumps the pinned container base images | The Trivy gate has no remediation path. Do it before the first deploy so the images going up are current. |
| **#145** | Web image ships a vulnerable openssl | Blocked upstream: no fixed node base image exists yet. Check `node:22-alpine3.24` before starting. |
| **#104** | README | The thing a reader meets first. Worth doing when the architecture has stopped moving. |

---

## After the deploy

Ordered within itself, but none of it gates the first environment.

| # | What |
|---|---|
| **#32** | OAuth2 social login — cut from pre-deploy deliberately |
| **#52**, **#53** | Realtime fan-out above a board, and telling a client the fan-out is down |
| **#136**, **#137** | VPC flow logs; ALB and S3 access logs |
| **#138** | SNS topic encryption — needs a KMS key policy grant, and getting it wrong silently breaks every alarm |
| **#139** | Read-only root filesystems on ECS tasks |
| **#140** | ElastiCache auth token — encrypted in transit, unauthenticated |
| **#30** | Fail the build when a job is missing from the `ci` aggregate's `needs` |
| **#154** | `pathUUID`'s 400 is untested, and #155's web copy depends on it |
| **#55** | `pgtest` naming — **re-scoped**, see audit notes |

---

## Audit notes

Issues whose description no longer matches the code. **Read these before
picking the issue up.**

### #55 — partially fixed, now a naming problem

`SchemaOwnerPool` exists and opens as `collabboard_owner`, the non-superuser, so
the correctness half is done. `OwnerPool` and `OwnerExec` still exist under names
that say "owner" and connect as the superuser. The remaining work is that
ambiguity, not a missing capability — a test that reaches for `OwnerPool`
expecting the schema owner gets the superuser and silently bypasses RLS.
Severity is lower than the issue implies; the trap is real.

### #87 — partially fixed, scoped to two forms

`maxLengthFor(codePoints)` exists in `lib/workspace/rules.ts` and
`board-controls.tsx` uses it. `create-project-form.tsx` and
`rename-project-form.tsx` still pass `MAX_NAME_CODE_POINTS` and
`MAX_DESCRIPTION_CODE_POINTS` directly. `add-member-form.tsx` passes
`MAX_EMAIL_BYTES`, which is a third unit again and worth a look while there.

### #56 — mostly done by #102

The issue was written when `infra/terraform/` was empty. Since then: both
Secrets Manager containers exist, the migrate and provision task definitions
exist, and the administrative task provides the psql session
`bootstrap-owner.sql` needs. What is actually left is the **rotation runbook**
(change the secret, run `api provision`, roll the tasks — in that order) and the
explicit decision about whether Terraform owns the lifecycle of the three
database roles. Re-read it against `modules/ecs` before starting.

### #105 — no longer blocked

Labelled `blocked` on #101, #102 and #103. All three are merged.
`infra/terraform/OPERATOR-INPUTS.md` is the raw material and already covers
#103's ten GitHub variables and the bootstrap ordering trap. The `blocked` label
should come off.

### #44 — still blocked, correctly

Blocked on #12, which is now in phase 2 rather than deferred.

### #73 — the comment it names is still there

`auth.go` still carries the comment claiming the registration endpoint is rate
limited by the per-address budget. Verify whether it became true before writing
the fix.

### Not stale

Everything not listed above was checked and still describes the code
accurately.
