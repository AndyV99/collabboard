# 0011. Terraform remote state, and a root module per environment

Date: 2026-08-10
Status: accepted

## Context

`infra/terraform/` was empty, so this is the first infrastructure in the project
and every convention it sets propagates into #102 (ECS + ALB), #103 (deploy
pipeline) and #56 (database role provisioning). Two things had to be decided
before any resource could be written: where state lives, and how one
configuration serves `dev`, `staging` and `prod`.

State is not an implementation detail here. Terraform state stores resource
attributes in plaintext, including ones redacted in plan output, so **the state
file is a credential store whether or not it is treated as one**. "Where does
state live and who can read it" is therefore the same question as "who can read
this project's secrets", and it has to be answered in the same breath as
creating the bucket.

The environment question is the one with a cheap-looking wrong answer. A single
root module selected by `-var-file=envs/staging.tfvars` is the smallest amount
of code, and the vault's plan says "environments driven by tfvars". But with a
single root there is exactly one state file per backend configuration, so the
environment is selected by two flags that must agree — `-backend-config` at init
and `-var-file` at plan — and neither Terraform nor CI will notice when they do
not. Workspaces have the same shape: one `terraform workspace select` away from
planning prod's variables against staging's state.

## Decision

**State lives in one S3 bucket per account, created by a separate `bootstrap/`
stack, encrypted with a customer-managed KMS key, versioned, and locked with
S3-native locking rather than a DynamoDB table.**

**Each environment is its own root module** under
`infra/terraform/environments/<env>/`, with its own `backend` block naming its
own state key, and its own auto-loaded `terraform.tfvars`. Shared resource
definitions live in `infra/terraform/modules/`. Only `staging` exists today.

Three parts are worth naming individually.

**The bootstrap stack keeps local state, and that is correct rather than
sloppy.** It creates the bucket that every other stack stores state in, so it
cannot store its own state there. Its state describes a bucket, a KMS key and an
alias — no credential — so a lost bootstrap state costs an import, not a leak.
That is a materially different risk from an environment's state, and the
asymmetry is the reason the split exists at all.

**No DynamoDB lock table.** Terraform 1.10 added S3-native locking
(`use_lockfile = true`), which takes the lock as a conditional PUT of
`<key>.tflock` in the state bucket itself. It removes a resource, removes a
second thing to pay for, and — the real argument — removes the possibility of
the lock table and the state bucket being granted to different sets of people,
which is a lock that can be bypassed by anyone who can write state without
being able to write the table. This sets the repo's Terraform floor at 1.10.

**A customer-managed key, not the AWS-managed `aws/s3` key.** The point is the
key policy: revoking it revokes the ability to decrypt every state file at once,
independently of S3 permissions and of the bucket policy. With an AWS-managed
key there is no such single lever. The bucket policy additionally denies any
`PutObject` encrypted with a different key, which bucket-default encryption
cannot prevent on its own — that is precisely the case where a state file would
end up readable by principals the key policy does not cover.

Today the key policy is AWS's default, which grants the account root and thereby
delegates authorization to IAM. The only principal in the account is the human
operator, so that is accurate rather than lax; it stops being accurate the
moment #103 introduces a GitHub OIDC deploy role, and narrowing it is called out
there.

## Consequences

**Adding an environment is a directory copy, and cannot be done by accident.**
`cp -r environments/staging environments/prod`, change `terraform.tfvars`,
change the `key` in the backend block. Two files, both reviewed in a diff. What
this buys is that there is no flag combination that plans prod against staging's
state, because the state key is written down next to the values it belongs with
rather than passed on a command line. What it costs is duplication: the module
wiring in `main.tf` is repeated per environment, so a new module has to be added
in each. At three environments that is acceptable; the moment it stops being
acceptable, the answer is a shared wrapper module, not workspaces.

**The bucket name cannot be committed, and this is a genuine rough edge.**
Backend blocks accept no variables, functions or references, and S3 bucket names
are globally unique, so the name has to include the account ID and cannot be
known when the code is written. The configuration is therefore *partial*: the
static half is in `versions.tf` and the account-specific half in a committed
`backend.hcl` carrying a `REPLACE_WITH_AWS_ACCOUNT_ID` placeholder, supplied via
`terraform init -backend-config=backend.hcl`. The bootstrap stack prints the
exact contents as an output so it is a paste rather than a reconstruction. A
forgotten edit fails at `init` against a nonexistent bucket, which is loud and
early — but it is still one manual step, and #105's runbook has to carry it.

**One bootstrap per account, not per environment.** All environments share the
bucket and the key, separated by state key prefix. That is fine while one person
holds the account and wrong the moment staging and prod need different readers,
because a single key policy cannot express "read staging state but not prod".
The exit is a bucket per account with environments in separate accounts, which
is the shape this would take anyway.

**Old state versions are retained for 90 days, then deleted.** They hold the
same secrets as current state, so an unbounded version history is an unbounded
archive of every credential the project has ever held. 90 days is long enough to
recover from a bad apply nobody noticed immediately.

**Reversal.** Cheap for the environment layout — collapsing three root modules
into one with `-var-file` is mechanical, and `terraform state pull`/`push` moves
the state. Expensive for the backend: migrating state to a different bucket or
key means `terraform init -migrate-state` on every environment, in lockstep,
with no apply running anywhere in between. The KMS key is the least reversible
piece, since rotating to a new key requires rewriting every object in the
bucket. That is why it is a decision rather than a default.
