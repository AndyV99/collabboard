# 0013. One-shot database work: migrations, provisioning, and how the first SQL ever reaches the database

Date: 2026-08-10
Status: accepted

## Context

#101 built the network and data layer and, in doing so, made the database
genuinely unreachable. The data subnet tier has no default route in either
direction, RDS is `publicly_accessible = false`, and the only ingress rule on the
database security group references the app security group — which, until #102,
nothing was attached to. That is the property the tier exists to have, and it is
asserted in `modules/network/tests`.

It also means three things that must happen cannot happen:

**`bootstrap-owner.sql` has nowhere to run from.** #56 creates
`collabboard_owner` — a non-superuser that owns the schema — by connecting as
the RDS master user and running a script. ADR 0001's tenant isolation rests on
that role existing, because `FORCE ROW LEVEL SECURITY` is only exercised when the
table owner is not the one running the queries. Without a Postgres connection
from inside the VPC there is no way to create it. #56 was filed as blocked on
#101; it is actually blocked on this issue.

**`api migrate up` has nowhere to run.** The migration chain is embedded in the
binary via `go:embed`, connects as the schema owner, and is refused outright if
the role it connects as is exempt from row-level security. Something has to run
it, once, against a database only reachable from inside the VPC.

**`api provision` has nowhere to run.** Per ADR 0006, the serving role's password
in Secrets Manager and the password in Postgres must be the same string, and the
thing that makes them the same is `api provision` — which connects as the owner
and `ALTER ROLE`s the serving role to the value in `POSTGRES_PASSWORD`.

There is a fourth constraint that shapes every answer. #101 made "the
application cannot connect as the RDS master user" structural rather than
conventional: `manage_master_user_password` keeps the credential with AWS, and
both ECS roles carry an explicit IAM `Deny` on its ARN with a `validation` block
that rejects an empty deny list. Any mechanism proposed here has to run
`bootstrap-owner.sql` *as the master user* without weakening that.

## Options

**A developer machine, with the database temporarily made public.** Flip
`publicly_accessible`, add an ingress rule for one address, run the script, put
it back. It is the fastest and it is the one that must not win: it turns a
property of the route table into a property of somebody remembering to revert
two settings, and the window it opens is exactly the window in which the master
credential is on a laptop. It also leaves the environment one forgotten
`terraform apply` away from a permanently public database.

**A bastion host.** A small EC2 instance in a private subnet, reached by SSM
Session Manager, with port forwarding to RDS. This is the textbook answer and it
genuinely works — SSM port forwarding to a remote host needs an SSM-managed
instance, and this is one. The costs are a t4g.nano at ~$3/month plus its EBS
volume, an operating system that now needs patching, and a standing host with a
route to the database that exists whether or not anybody is using it. For a
staging environment whose whole cost argument (ADR 0012) is about not paying for
things that sit idle, a permanently running instance to support an operation
performed once per environment is poor value.

**A one-off Fargate task, with ECS Exec.** A task definition that exists but runs
nothing until an operator starts it. It costs nothing when idle, has no operating
system to patch, and disappears on its own. ECS Exec cannot do port forwarding —
it only offers interactive commands — so the task has to carry a Postgres client
rather than tunnel to one. That is not a limitation in practice: a stock
`postgres:16-alpine` image from ECR Public is exactly the tool wanted, and
Fargate pulls it anonymously.

The remaining question is how the master password reaches that task. Giving the
task an execution role that can read the secret would be conventional, and would
create a machine identity in the account capable of reading the credential that
bypasses row-level security. The alternative is that the operator reads it with
their own IAM identity and types it at `psql`'s prompt.

## Decision

**Four one-shot task definitions, no bastion, and no role in the ECS layer that
can read the RDS master credential — including the one whose job is to use it.**

**`<prefix>-admin`** is the break-glass path. `public.ecr.aws/docker/library/postgres:16-alpine`,
`sleep 3600`, its own security group that reaches Postgres and nothing else
(not Redis), and a task role holding exactly four `ssmmessages` actions plus
permission to write its own Exec session transcript. It carries **no `secrets`
entries at all**, and its task role carries the same `Deny` on the master
credential as every other ECS role. The operator runs:

```
aws secretsmanager get-secret-value --secret-id "$(terraform output -raw database_master_user_secret_arn)"
aws ecs run-task --enable-execute-command ...
aws ecs execute-command --interactive --command /bin/sh ...
psql -U collabboard_master -W        # -W prompts; the password is never on a command line
```

`sleep 3600` rather than an indefinite command is deliberate: a break-glass path
that has to be cleaned up by hand is one that gets left running.

**`<prefix>-api-migrate`** runs `api migrate up`, and **`<prefix>-api-provision`**
runs `api provision`. Both use the API image and the existing execution role,
which already reads secrets under `collabboard/<env>/`. Migrate gets the owner's
password only; provision gets the owner's and the serving role's, because setting
one requires connecting as the other. Neither is ever the serving task: a serving
task with the owner's password could run DDL, and asserting that it does not have
one is a test in `modules/ecs/tests`.

Both also carry `AUTH_JWT_SECRET`, which neither uses. `config.Load` runs before
the subcommand is dispatched and refuses to return a configuration without one
outside development, so omitting it produces a migration that exits 1 complaining
about a signing key — which reads like the wrong task definition was run.

**Migrations run once, before the service is rolled**, from #103's pipeline, not
from a container entrypoint. An entrypoint migration runs once per task, so three
tasks rolling is three concurrent attempts whose safety depends entirely on
goose's advisory lock holding. Running it exactly once means the question does
not arise.

**When a migration fails halfway, nothing rolls forward and nothing rolls back
automatically.** goose runs each migration in its own transaction, so a failure
leaves every migration before it applied and the failing one reverted — the
schema is at a real version, not a torn one. The `run-task` exits non-zero, the
pipeline stops, and the currently running tasks keep serving the *old* code
against the *partially new* schema. That is survivable only if migrations are
backward-compatible with the code already running, which is the actual
requirement this decision imposes: **expand, deploy, contract, in three separate
deploys, never one.** A migration that drops a column the running code still
selects turns a failed deploy into an outage regardless of what the pipeline
does next.

There is deliberately no automatic `api migrate down`. A down migration on a
production database is a data-loss operation being performed by a robot at the
exact moment nobody understands the state; the correct response to a failed
migration is a human reading the log, which is why the migration task writes to
the API's log group rather than somewhere separate.

## Consequences

**#56 is unblocked and has something concrete to use.** Its script runs from the
`<prefix>-admin` task. Its two secrets, `collabboard/<env>/db/app` and
`collabboard/<env>/db/owner`, are now created empty by #102 — a task definition's
`secrets:` entry needs an ARN at plan time — so #56 writes values into containers
that already exist rather than creating them. A task started before #56 has run
fails with `ResourceInitializationError`, which is an accurate way to say "nobody
has provisioned this environment yet".

**The master credential remains a human-only capability.** Nothing in the account
that is not a person can read it. That is a stronger position than the
conventional one and it costs a password prompt.

**ECS Exec sessions are transcribed to CloudWatch and retained for a year.** The
cluster is configured with `logging = "OVERRIDE"`. This has a sharp edge worth
stating: anything visible in the session is written to that log group, so a
password pasted onto a command line would be recorded. `psql -W` prompts without
echoing, which is why the runbook says to use it. The transcript is deliberately
not encrypted with the environment CMK — the channel is TLS-encrypted regardless,
and doing so would require the admin task role to hold `kms:GenerateDataKey` on a
key whose every other grant is conditioned on a specific service.

**A capability that #101 declined now exists, on one role.** #101 left
`ssmmessages` off the API's task role and recorded that the decision belonged
here. It is granted only to the administrative task role, and
`enable_execute_command` is explicitly `false` on both long-running services.
`modules/iam/tests` asserts both halves, so switching Exec on for the serving
tasks fails a test rather than passing review.

**Rotating the serving role's password is a deploy, not an API call.** Change the
secret, run `api provision`, roll the tasks — in that order, because changing the
password invalidates it for every task still holding the old one. This was
already ADR 0006's position; #102 is where it becomes three `aws ecs` commands
rather than a paragraph.

**Cost is zero when nothing is running**, which is the whole argument for this
over a bastion. An hour of the administrative task is roughly one cent.
