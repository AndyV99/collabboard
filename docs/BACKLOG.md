# Ordered backlog

Every open issue, checked against the code as it stands on `main` and ordered
toward one goal: **an app that is worth deploying, deployed once, and not
before.** Nothing here is applied to AWS yet, deliberately — staging costs about
$107/month running, and the app is still changing.

Audited 2026-09-02 at `0139c9d`. **35 open issues.**

## How to use this

Work top to bottom within a phase. **Within a phase, items with no "after" line
can run in parallel**; anything with one has to wait. Phases are ordered by
dependency, not by importance — Stripe is the headline feature and it is in
phase 4 because the three phases before it are what make it safe to build.

Each entry says what it is, why it sits where it does, and what it depends on.
The issue itself carries the detail; this file carries the order.

**Before starting anything here, re-read the issue, then read the "Audit notes"
section at the bottom.** Several issues were written against code that has since
moved, and two of them are now materially smaller than they read.

## What changed since the 2026-08-29 audit

Seven issues closed: **#157** (assignee and due date on the board), **#80**
(the proxy relays the API's own status), **#87** (`maxLength` derived from the
right unit), **#78** (the viewer read moved from `/members` to `/me`), **#84**
(the router warns when it can serve no `/api/v1` route), **#67**
(`organization_name` bounded, plus a schema CHECK), and **#86** (an
authenticated account can create up to five workspaces).

Two filed: **#164** and **#168**, both noticed while doing the above and both
deliberately not folded into the PR that surfaced them.

**#67 was pulled forward out of phase 5**, because #90 cannot be done without
it — the previous audit missed that dependency. #90 is the only phase-1 item
left, and it is now unblocked.

## The target

Usable means: a person can sign up, make a workspace — or a second one — invite
somebody, run a board, and pay for it. A card says who has it and when it is
due; the board says who changed what, live.

Two things stay deliberately cut from the pre-deploy set:

- **OAuth2 social login (#32)** — email/password auth already works and is
  carefully built. OAuth adds a second credential story to the part of this
  codebase with the most delicate invariants, for impressiveness rather than
  usability.
- **Realtime fan-out above a board (#52, #53)** — board and project creation are
  not announced live. Visible only if two people are looking at the same project
  list at the same moment.

---

## Phase 1 — Finish making what exists correct

One item left of the seven this phase started with.

| # | What | Why here |
|---|---|---|
| **#90** | An organization's name is permanent | There is still no `PATCH` under `/organizations` — `router.go` mounts exactly two, both `POST`. The absence is conspicuous next to project and board rename, and `workspace-recovery.tsx` currently has to promise a name "cannot be renamed later". **Now unblocked**: its `#67` dependency merged, and `validateWorkspaceName` exists in `internal/auth/service.go`, so a rename cannot reintroduce the unbounded field. |

---

## Phase 2 — Make the running system legible

Nothing after this phase is debuggable without it, and none of it can be
retrofitted cheaply once there is traffic. This is also the phase that makes the
first deploy worth watching rather than worth hoping about.

| # | What | Why here |
|---|---|---|
| **#95** | Almost no log line carries `request_id` | The foundation. Verified: **four** non-test lines in `internal/` carry one — the request logger's own, one in `movelog.go` and two in `crud.go`. Do this first; #96 and #97 are both "log the thing that currently logs nothing", and they want the correlation id to already exist. |
| **#96** | A cross-tenant probe leaves no trace | `notFound`/`asNotFound` in `crud.go` log nothing at all, so the one attack this architecture is built to resist is invisible. *After #95.* |
| **#97** | The three 409s from `writeAuthError` log nothing | Verified: no logging call anywhere near them. A rejected registration or member add is untraceable. *After #95.* |
| **#12** | RED/USE metrics and a Prometheus `/metrics` endpoint | There is no `/metrics` route and no Prometheus dependency. `modules/ecs` sets `container_insights = false` **specifically because** this was the plan, so deploying without it means no application metrics at all. The ALB listener rules already 404 `/metrics` publicly, so the route has somewhere to live. |
| **#44** | Hub metrics — connections, rooms, drops by close code | The realtime half of the same picture. *After #12.* Correctly labelled `blocked`. |

---

## Phase 3 — A net under the next two phases

| # | What | Why here |
|---|---|---|
| **#17** | Playwright harness + first smoke test | **Before Stripe, not after.** There is no `playwright.config`, no `e2e/` directory and no dependency yet. Stripe is the largest remaining change and touches auth, a migration and a new unauthenticated route; having e2e in place means it is protected while it is built rather than tested afterwards. It also gives #154's and #91's untested claims somewhere to land, and it becomes the post-deploy smoke check `scripts/ci/smoke.sh` already exists for. |

---

## Phase 4 — Stripe

The headline feature. Nothing exists yet — no dependency, no billing routes, no
schema. Filed as three so they can be worked separately.

| # | What | Why here |
|---|---|---|
| **#160** | The subscription model, and the organization ↔ customer link | The migration and the data model. Everything else in this phase reads what this writes. |
| **#161** | Webhook endpoint: signature verification and idempotency | *After #160.* Three hazards named in the issue: signature verification must run against the **raw** body, which collides with the existing body-limit middleware; dedupe and state change must share a transaction; out-of-order delivery must not let a stale event downgrade a paying customer. `CLAUDE.md` singles this out for review. |
| **#162** | A billing screen — plan, seat usage, upgrade path | *After #160.* The web half. |

---

## Phase 5 — What the deploy should not go without

| # | What | Why here |
|---|---|---|
| **#73** | The address-existence oracle is unbudgeted | **Verified false comment** — see audit notes. `Register` does not call the rate limiter; only `Login` and `CreateFirstOrganization` do. The 409 on a duplicate address is therefore an unbudgeted enumeration oracle, and `auth.go` claims otherwise in a comment. |
| **#71** | No request body size limit on the web proxy or the auth handlers | Verified: `app/api/proxy/[...path]/route.ts` calls `await request.text()` with no guard, and the auth routes go through `readJsonBody` → `request.json()`. The Go API got its limits in #50; the Next tier in front of it never did, and since #86 it fronts one more authenticated route. |
| **#69** | Refresh-token rotation races across processes | Verified: `lib/session/refresh.ts` single-flights through a module-level `Map`, which is per-process. **This blocks web autoscaling** — `web_desired_count` is pinned at 1 with a comment saying "while #69 is open". |
| **#91** | `/api/auth/login`'s promise that it logs no email is not tested | The promise is in the code (`route.ts`, "No log line naming the address"). No test asserts it. A comment is not a control. |
| **#55** | `OwnerPool` and `OwnerExec` name the superuser | **Now unblocked and three lines of work** — see audit notes. |
| **#154** | `pathUUID`'s 400 for a malformed id is untested | Verified: `pathUUID` appears in no test file, and the web copy now depends on that 400 (`bad_request` in `describeLoadFailure`). |
| **#117** | Any file named `.gitleaks.toml` is silently skipped | Including by the pre-commit hook. The config documents the behaviour at line 34; nothing works around it. |
| **#114** | The libpq keyword/value DSN form is not caught | Verified: one rule id, `collabboard-datastore-url-inline-credential`, covering the URL form only. |
| **#116** | `.gitleaks.toml` has no regression test | Verified: `scripts/ci/` holds `deploy-service.sh`, `run-one-shot-task.sh`, `smoke.sh` and nothing else. A rule can be silently broken. *Do after #114 and #117, so the test covers the rules they add.* |

---

## Phase 6 — Deploy

The first `terraform apply`. Everything above is done and the bill starts here.

**Register a domain in Route 53** is an operator action, not an issue — do it
first, because certificate validation is the long pole.

| # | What | Why here |
|---|---|---|
| **#56** | Provision the schema owner and the app-role secret | Mostly delivered by #102 — see audit notes. What is left is the rotation runbook and the role-lifecycle decision. |
| **#105** | Infra runbook, empty account to running staging | **The `blocked` label should come off** — its three dependencies all merged. `infra/terraform/OPERATOR-INPUTS.md` is the raw material. |
| **#137** | The load balancer and both S3 buckets keep no access log | Verified: no `access_logs` anywhere in `infra/terraform`. Pulled forward from after-deploy: it is a bucket and a policy, and applying it with everything else is cheaper than a second apply. |
| **#139** | ECS tasks run with a writable root filesystem | Verified: no `readonly_root_filesystem` anywhere. Same reasoning as #137, plus this is the one that is genuinely painful to retrofit — you find out what writes where by breaking it. |
| **#145** | Web image ships openssl 3.5.7-r0 (CVE-2026-14456) | The `.trivyignore` entry expires **2026-11-28**. Check whether a fixed `node:22-alpine3.24` exists before the deploy; if it does, this is a one-line bump. |
| **#144** | Nothing bumps the pinned container base images | Verified: `dependabot.yml` covers `gomod`, `npm` and `github-actions` — **no `docker` ecosystem**. Without it the Trivy gate has no remediation path, which is how #145 becomes permanent. |
| **#104** | README: architecture diagram, setup that works, decisions | Verified: the root README is 307 lines with **no diagram**. Do it last, when the thing it describes has stopped moving. |

---

## After the deploy

Real, and none of it is what makes the app usable or safe to run.

| # | What |
|---|---|
| **#136** | No VPC flow logs. Costs money per GB and is easier to size once there is traffic to size it against. |
| **#138** | Alarm notifications are unencrypted. The SNS topic is real (`modules/alb/main.tf:363`); the work is the KMS key policy grant, which the issue correctly calls the hard part. |
| **#140** | ElastiCache has no auth token. **Deliberate, not an oversight** — see audit notes. Effectively after #56. |
| **#32** | OAuth2 social login. |
| **#52** | No fan-out unit above a board, so board and project creation cannot be announced. Verified: `events.go` has `board.updated` and `board.deleted` and nothing above them. |
| **#53** | A client is never told when the fan-out is unavailable. *After #52.* |
| **#30** | Fail the build when a job is missing from the `ci` aggregate's `needs`. **The list is complete today** — see audit notes. |
| **#164** | Every timestamp but the due date is shown in UTC, to every reader. |
| **#168** | The protected layout's viewer request blocks every `loading.tsx` below it. |

---

## Audit notes

Issues whose description no longer matches the code. **Read these before
picking the issue up.**

### #55 — now unblocked, and three call sites

The issue reads as a rename with unclear scope. It is smaller than that, and its
own blocker is gone.

`OwnerPool`'s doc comment says: *"Kept only because internal/api spells it this
way and #45 is in flight over that package. Do not use it in new code; it goes
away once that lands."* **#45 is closed.** The remaining callers are exactly
three:

- `internal/api/crud_integration_test.go:150` — `testDB.OwnerPool(t, 2)`
- `internal/api/crud_integration_test.go:964` — `testDB.OwnerPool(t, 2)`
- `internal/realtime/realtime_integration_test.go:346` — `testDB.OwnerExec(...)`

Decide per call site whether it wants the superuser (`SuperuserPool`) or the
schema owner (`SchemaOwnerPool`) — they are not interchangeable, and picking the
wrong one silently bypasses RLS, which is the whole trap — then delete both
misnamed methods.

### #73 — the comment is false, verified

The previous audit said to check whether it had become true. It has not.
`s.limiter.Allow` is called in exactly two places: `Login`
(`service.go:513`) and `CreateFirstOrganization` (`organizations.go:200`).
`Register` calls neither. So the comment above `registerHandler`
(`internal/api/auth.go:236`) — *"The registration endpoint is rate limited by
the same per-address budget"* — is wrong, and the 409 it justifies is an
unbudgeted address-existence oracle.

Fix the code, then the comment. Not the other way round.

### #56 — mostly done by #102

The issue was written when `infra/terraform/` was empty. Since then both Secrets
Manager containers exist, the migrate and provision task definitions exist, and
the administrative task provides the psql session `bootstrap-owner.sql` needs.
What is left is the **rotation runbook** (change the secret, run `api provision`,
roll the tasks — in that order) and the explicit decision about whether Terraform
owns the lifecycle of the three database roles.

### #105 — no longer blocked

Labelled `blocked` on #101, #102 and #103. All three are merged. The label
should come off.

### #140 — a documented decision, not an omission

The issue reads as though the missing `auth_token` were an oversight.
`modules/cache/main.tf:43` says otherwise, at length: Redis AUTH would add a
second credential whose provisioning and rotation has the same shape as the
database secrets in #56, and it defends against an attacker who already has a
route to the node — which requires membership in the app security group. The
comment says *"Worth revisiting when #56 has established the pattern"*, and
points at ADR 0012.

So this is a **deferral with a named trigger**, and the issue should either be
re-scoped to "revisit after #56" or closed as answered by the ADR.

### #30 — the hole is not open, only unguarded

The `ci` aggregate's `needs` is `[changes, api, integration, web, image, infra,
secret-scan]`, which is every job in the workflow — checked against the job list
today. There is even a comment naming #30.

A comment is not a check. The issue is about making a *future* omission
impossible, not about a gap that exists now, and it should be read that way: the
acceptance criterion is a failing build when a job is left out, not a corrected
list.

### #44 — still blocked, correctly

Blocked on #12, which is in phase 2.

### Not stale

Everything not listed above was checked against the code and still describes it
accurately. The checks that mattered: no `/metrics` route or Prometheus
dependency (#12); no Playwright config or `e2e/` directory (#17); no Stripe
dependency, route or table (#160–#162); no `flow_log` (#136), `access_logs`
(#137) or `readonly_root_filesystem` (#139) in the Terraform; no `docker`
ecosystem in `dependabot.yml` (#144); no `PATCH` under `/organizations` (#90);
`pathUUID` in no test file (#154); and the login route's "no log line naming the
address" promise still untested (#91).
