# Operator inputs — raw material for #105

**This is not the runbook.** #105 is the runbook. This file is the inventory it
should be built from and cross-checked against: what a person has to supply,
what holds a secret and who writes it, which steps Terraform cannot do, and what
keeps billing after `destroy`. It was written alongside the Terraform in #101
rather than reconstructed from it afterwards.

Scope: the base infrastructure in #101 only. The ECS service and ALB (#102), the
deploy pipeline (#103) and the database role provisioning (#56) each add their
own rows, and this file should grow with them rather than being rewritten.

No real secret value appears here, and none should be added — where a value is
needed, the command that produces it is given instead.

---

## 1. Values a person must supply

**Nothing is prompted for.** Every variable without a default is set in
`environments/staging/terraform.tfvars`, which Terraform auto-loads, so `plan`
and `apply` run without interactive input and without a `-var-file` flag. The
list below is what those values mean and what breaks if one is wrong — it is a
review checklist, not a set of prompts.

The `bootstrap/` stack has **no required variables at all**: `aws_region`
defaults to `us-east-1` and `project` to `collabboard`.

| Variable | Set in tfvars to | What it means | If it is wrong |
|---|---|---|---|
| `aws_region` | `us-east-1` | Region for every resource | Must equal the hardcoded `region` in the `backend` block in `versions.tf` — a backend block accepts no variables, so the two are duplicated and can drift. A mismatch puts state in one region and resources in another; both work, which is why nothing complains. |
| `environment` | `staging` | Second component of every resource name; the `Environment` tag | Changing it after apply renames nearly every resource, which for RDS and ElastiCache means replacement — i.e. data loss. Fix before first apply or not at all. |
| `vpc_cidr` | `10.20.0.0/16` | VPC address space | Must not overlap another environment or anything peered later. Immutable after creation: changing it recreates the VPC and everything in it. Must be `/20` or larger (validated). |
| `availability_zones` | `["us-east-1a","us-east-1b"]` | AZs to spread subnets across | Must be two or more, in `aws_region`, and must be AZs the account actually has — AZ names are per-account aliases, and not every account has every AZ. A nonexistent AZ fails at apply. Reordering the list re-indexes subnets and recreates them. |
| `nat_gateway_count` | `1` | NAT gateways, 0–2 | **The largest cost lever: ~$32.85/mo each.** `0` costs nothing and is correct until #102 exists. Below `1`, a Fargate task cannot pull its image and hangs in `PROVISIONING` with `CannotPullContainerError`, which does not read as a network problem. |
| `db_instance_class` | `db.t4g.micro` | RDS sizing | Must be a class the region offers for Postgres 16. Also gates Performance Insights: AWS does not support it on `db.t2/t3/t4g.micro` or `.small`, so enabling `performance_insights_enabled` on this class is rejected at **plan** time by a variable validation, rather than 30 seconds into a 15-minute create. `db.t4g.medium` is the first class where it works, at roughly 4x the cost. |
| `db_max_allocated_storage` | `50` | Storage autoscaling ceiling, GB | Storage autoscaling is **one-way** — RDS never scales back down — so this, not `db_allocated_storage`, is the real upper bound on the storage bill: $5.75/mo at 50 GB versus $2.30 at 20. Set it equal to `db_allocated_storage` to disable autoscaling and accept that a full disk takes the database down instead. |
| `db_multi_az` | `false` | RDS standby in the second AZ | `true` doubles the instance cost. Converting either way is an online modification, so this one is safe to change later. |
| `db_deletion_protection` | `false` | Blocks instance deletion | `true` makes `terraform destroy` fail — which is the point in prod and the wrong default for a demo environment you want to switch off. |
| `db_skip_final_snapshot` | `true` | Skip the snapshot taken on destroy | `true` **discards the database on destroy with no recovery**. Correct for staging, wrong anywhere with data worth keeping. |
| `cache_node_type` | `cache.t4g.micro` | ElastiCache sizing | Must be a current-generation type; `transit_encryption_enabled` is not supported on the oldest ones. |
| `cache_node_count` | `1` | Cache nodes | `1` disables automatic failover (Terraform derives `automatic_failover_enabled` and `multi_az_enabled` from it). Each node above the first is a full node's cost. |

Variables with defaults, worth knowing exist: `project` (`collabboard`),
`db_allocated_storage` (20 GB), `db_backup_retention_days` (7),
`attachments_force_destroy` (`true` in staging), `apply_immediately` (`true` in
staging).

### 1.1 Conventions invented here that later issues must honour

Neither is validated by anything; both propagate.

- **ECR repository names are slash-separated.** The execution role's pull
  permission is scoped to `repository/collabboard/*`, so #103 must create
  `collabboard/api`, not `collabboard-api`. A mismatch denies the pull and
  surfaces as `CannotPullContainerError` — indistinguishable at a glance from
  the missing-NAT case above.
- **Secrets live under `collabboard/<environment>/`.** The execution role can
  read anything under that prefix and nothing outside it, so #56's secrets must
  be named `collabboard/staging/db/app` and `collabboard/staging/db/owner` or
  the IAM policy needs widening too.

---

## 2. Secrets: who writes, who reads, how it rotates

The three-way distinction is the thing to get right. In this stack, **Terraform
writes no secret value at all** — there is no `random_password` resource and no
variable that accepts a credential, which is what makes committing
`terraform.tfvars` safe.

### 2.1 RDS master user password

| | |
|---|---|
| **Where** | Secrets Manager. Name is generated by AWS in the form `rds!db-<db-resource-id>` — it is not chosen, and not predictable before apply. ARN is the `database_master_user_secret_arn` output. |
| **Written by** | **AWS (RDS)**, via `manage_master_user_password = true`. Not Terraform and not a human. The value never enters a plan, an output, or Terraform state. |
| **Read by** | A **human operator**, once, to run `bootstrap-owner.sql` in #56. Nothing else. Both ECS roles carry an explicit IAM `Deny` on this ARN (`modules/iam/main.tf`), so the application cannot read it even if a future task definition names it. |
| **Encrypted with** | The environment data CMK, `alias/collabboard-staging-data`. Note this is a *different* key from the state key created by `bootstrap/`. |
| **Rotation** | RDS-managed, not Terraform: `aws rds modify-db-instance --db-instance-identifier collabboard-staging-postgres --rotate-master-user-password --apply-immediately`. No Terraform change and no redeploy. |
| **To read it** | `aws secretsmanager get-secret-value --secret-id "$(terraform output -raw database_master_user_secret_arn)" --query SecretString --output text` — returns JSON with `username` and `password`. |

### 2.2 `collabboard_app` and `collabboard_owner` passwords

**These do not exist yet.** #56 creates them; #101 only pre-authorises them.

- Intended names: `collabboard/staging/db/app` and `collabboard/staging/db/owner`.
- The execution role is already scoped to `secretsmanager:GetSecretValue` on
  `collabboard/staging/*` (the `secret_name_prefix` output), so #56 adds secrets
  without also having to widen an IAM policy.
- **Who writes them is #56's decision and is not yet made.** Per ADR 0006 the
  value in Secrets Manager and the value in Postgres must be the same string, so
  whoever generates it must also be the thing that runs `api provision`.
- Read by: the **execution role** only (the ECS agent, before the container
  starts). The task role has no `secretsmanager` permission at all, so the
  running application receives the password as an environment variable and
  cannot re-read, rotate or enumerate secrets.
- Rotation is a deploy, not an API call: change the secret, run `api provision`,
  roll the tasks, in that order (ADR 0006).
- Generating one, for whoever writes the runbook:
  `aws secretsmanager get-random-password --exclude-punctuation --password-length 32 --query RandomPassword --output text`

### 2.3 Terraform state

Not a named secret, but it holds resource attributes in plaintext including ones
redacted in plan output. Treat the state bucket as a credential store.

- **Environment state**: `s3://collabboard-tfstate-<account-id>/staging/base/terraform.tfstate`, versioned, encrypted with `alias/collabboard-tfstate`.
- **Who can read it**: anyone with S3 read on the bucket *and* `kms:Decrypt` on that key. Today the key policy is AWS's default, which delegates to IAM — so in practice, any admin identity in the account. Narrowing this is called out in ADR 0011 and belongs with #103's OIDC role.
- **Bootstrap state is local and gitignored.** It describes a bucket, a KMS key and an alias — no credential — so losing it costs an import, not a leak. That asymmetry is why bootstrap is a separate stack.

### 2.4 The operator's own AWS credentials

Never in this repository, never in a tfvars. A named CLI profile or SSO session.
`aws sts get-caller-identity` is the check that one is active.

---

## 3. What Terraform cannot do

Four things, three of them chicken-and-egg.

1. **An AWS account and an identity that can create IAM roles, KMS keys, VPCs, RDS and S3.** Must exist before the first `init`. Nothing in this repo creates it.
2. **The bootstrap apply.** `bootstrap/` creates the S3 bucket that every environment stores state in, so it cannot store its own state there. It runs first, with local state.
3. **Filling in `environments/staging/backend.hcl`.** The bucket name contains the AWS account ID and the KMS key ARN contains a generated UUID, and a `backend` block accepts no variables, functions or references — so neither can be computed and both must be pasted. `bootstrap`'s `backend_config` output prints the exact contents. **`kms_key_id` must be the key ARN, not the `alias/...` name**: the S3 backend rejects an alias, and the state bucket's `DenyWrongKmsKey` policy compares the request header against the exact ARN, so an alias would 403 every state write *and* every `.tflock` acquisition — a permanent lockout whose error message says "Access Denied" and points at IAM.
4. **Everything in #56.** `bootstrap-owner.sql` needs a Postgres connection, and there is no path to the database after #101 — the data subnets have no ingress except from the app security group, and there is no bastion, no VPN and no ECS task yet. **#56 is therefore blocked on #102, not only on #101**, which is not what #56 currently says.

---

## 4. Order of operations, with the check for each step

Each check answers "did that actually work" before the next step depends on it.

| # | Do | Verify |
|---|---|---|
| 0 | Have credentials active | `aws sts get-caller-identity` prints an account ID |
| 1 | `cd infra/terraform/bootstrap && terraform init` | "Terraform has been successfully initialized" |
| 2 | `terraform apply` | `aws s3api get-bucket-versioning --bucket <name>` prints `Enabled`; `aws s3api get-public-access-block --bucket <name>` prints four `true`s |
| 3 | `terraform output -raw backend_config` → paste into `environments/staging/backend.hcl` | The file contains a 12-digit account ID, a `kms_key_id` starting `arn:aws:kms:`, and no `REPLACE_` string |
| 3.5 | Prove the bucket accepts an encrypted write before trusting it with state: `aws s3api put-object --bucket <state-bucket> --key .smoke --server-side-encryption aws:kms --ssekms-key-id <key-arn>` | Returns an ETag. An `AccessDenied` here means the key ARN and the bucket policy disagree, and catches the lockout at step 3 instead of step 5. Delete the object afterwards. |
| 4 | `cd ../environments/staging && terraform init -backend-config=backend.hcl` | "Successfully configured the backend \"s3\"". A wrong or missing bucket fails here, loudly |
| 5 | `terraform plan` | Reviewed by a human. Expect ~50 resources and **no** `random_password`, no `aws_secretsmanager_secret_version` and nothing marked `(sensitive value)` in a `value =` position |
| 6 | `terraform apply` | RDS reaches `available` (10–15 min; it is the long pole) |
| 7 | Confirm nothing holding data is reachable | `aws rds describe-db-instances --db-instance-identifier collabboard-staging-postgres --query 'DBInstances[0].PubliclyAccessible'` → `false`; the data route table has only the `local` route; `aws s3api get-public-access-block` on the attachments bucket → four `true`s |
| 8 | Confirm the ADR 0001 guard is real | `aws iam simulate-principal-policy --policy-source-arn "$(terraform output -raw ecs_task_role_arn)" --action-names secretsmanager:GetSecretValue --resource-arns "$(terraform output -raw database_master_user_secret_arn)"` → `explicitDeny`. Repeat with `ecs_task_execution_role_arn`. |
| 9 | Confirm the guard did not overshoot | Same command shape, `--action-names s3:PutObject --resource-arns "arn:aws:s3:::$(terraform output -raw attachments_bucket_name)/*"` on the **task** role → `allowed`. A Deny or an implicit deny here means the bucket policy or the KMS condition is too tight, which would break every attachment upload in #102. |

Steps 8 and 9 are the pair worth keeping, and they belong together: 8 proves the
isolation guarantee is in effect rather than merely in the code, and 9 proves it
was bought without breaking the thing the role exists to do. Both are single
commands and neither changes anything.

`modules/iam/tests` already asserts that the Deny exists, names the master
secret's ARN, covers every `secretsmanager` action and is attached to both
roles — but it does that against a mocked provider, so it checks the policy as
*written*. Step 8 is the only thing that checks how AWS *evaluates* it. Keep
both; they answer different questions.

---

## 5. Teardown — what `destroy` leaves behind

`terraform destroy` in `environments/staging` removes the VPC, RDS, ElastiCache,
S3 attachments bucket, IAM roles, the API log group and the NAT gateway with its
Elastic IP. **These survive it, and three of them keep billing:**

| Survives | Bills? | Why, and how to clear it |
|---|---|---|
| The environment KMS key | **Yes, ~$1/mo** | Enters a 30-day pending-deletion window and bills throughout. `aws kms cancel-key-deletion` reverses it; nothing shortens it. |
| `/aws/rds/instance/collabboard-staging-postgres/postgresql` | **Yes**, per GB stored | Created by RDS, not by Terraform, because `enabled_cloudwatch_logs_exports` is set. Default retention is **never expire**. Delete with `aws logs delete-log-group`. |
| The whole `bootstrap/` stack | **Yes, ~$1/mo** for its KMS key | `prevent_destroy = true` on the bucket and key, so `destroy` fails by design. Tearing it down means editing the `lifecycle` blocks, deleting every object *version* in the bucket, then destroying — deliberately awkward. |
| Manual RDS/ElastiCache snapshots | **Yes**, per GB-month | Never touched by `destroy`. `staging` takes no *final* snapshot (`db_skip_final_snapshot = true`), but any snapshot taken by hand persists. In an environment where `db_skip_final_snapshot = false`, the final snapshot survives on purpose and is named `<prefix>-postgres-final-<8 hex chars>`. |
| Deleted Secrets Manager secrets | Small | Sit in a 7–30 day recovery window and bill at $0.40/secret-month until purged. `aws secretsmanager delete-secret --force-delete-without-recovery` purges immediately. |

Not a problem, worth knowing: automated RDS backups are removed
(`delete_automated_backups = true`), the attachments bucket deletes with objects
still in it (`attachments_force_destroy = true` in staging — this would fail in
prod), and the Elastic IP is released with its NAT gateway rather than lingering
as an unattached address, which is the classic surprise charge.

**The cheapest way to stop most of the bill without a full teardown** is
`nat_gateway_count = 0` plus `terraform apply` — that is ~$33/mo of the ~$61/mo
gone in one variable, leaving the data intact.
