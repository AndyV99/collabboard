# Operator inputs — raw material for #105

**This is not the runbook.** #105 is the runbook. This file is the inventory it
should be built from and cross-checked against: what a person has to supply,
what holds a secret and who writes it, which steps Terraform cannot do, and what
keeps billing after `destroy`. It was written alongside the Terraform in #101
rather than reconstructed from it afterwards.

Scope: the base infrastructure in #101, plus the load balancer, ECS services and
one-shot task definitions in #102. The deploy pipeline (#103) and the database
role provisioning (#56) each add their own rows, and this file should grow with
them rather than being rewritten.

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

### 1.0.1 Added by #102

The first three are the only values in the whole configuration that describe
something outside this repository, and all three are placeholders that must be
changed before the first apply.

| Variable | Set in tfvars to | What it means | If it is wrong |
|---|---|---|---|
| `web_hostname` | `staging.collabboard.example.com` | The product's public name | **Placeholder.** `example.com` is RFC 2606 reserved so a forgotten one cannot resolve to somebody's real domain. Must be inside `route53_zone_id`'s zone, or certificate validation never completes. |
| `api_hostname` | `api.staging.collabboard.example.com` | The API's name. Resolves publicly; the listener answers only `api_ingress_cidrs` | **Placeholder.** Must differ from `web_hostname` (validated) — they are served by different listener ports with different exposure. |
| `route53_zone_id` | `Z00000000000000000000` | Hosted zone Terraform writes the ACM validation and alias records into | **Placeholder.** Must be a zone **ID**, not a name (validated by shape). The zone must exist in Route 53 in this account: this is the one place the configuration requires the operator's domain to live there. A wrong zone fails during `aws_acm_certificate_validation`, which waits several minutes and then reports a not-found error that reads like a permissions problem. |
| `api_admin_ingress_cidrs` | `[]` | Extra addresses allowed to reach the API listener | Empty is the setting that means "the API is not on the internet". `0.0.0.0/0` is rejected by a validation — see ADR 0014. |
| `alb_idle_timeout_seconds` | `120` | Seconds of silence before the load balancer closes a connection | **The setting that kills realtime silently.** Validated against `realtime_ping_interval_seconds + realtime_pong_timeout_seconds`; below twice their sum it is a plan-time error. AWS's own default is 60, which works today and would stop working the moment the ping interval was raised. |
| `realtime_ping_interval_seconds` / `realtime_pong_timeout_seconds` | `25` / `10` | `REALTIME_PING_INTERVAL` and `REALTIME_PONG_TIMEOUT` in the task definition, and the floor the idle timeout is checked against | One variable each, feeding both places, so the load balancer cannot be tuned for a ping interval the application is not using. |
| `api_image_tag` / `web_image_tag` | `bootstrap` | Image tags the task definitions name | **An image must exist at each tag before the first apply.** Nothing in this repository builds one — the API has no Dockerfile at all yet. With `wait_for_steady_state`, apply otherwise fails after ~10 minutes with `CannotPullContainerError`. |
| `api_cpu` / `api_memory` / `web_cpu` / `web_memory` | `256` / `512` | Fargate sizing. 256 CPU units = 0.25 vCPU | Must be a combination Fargate accepts. At 0.25 vCPU an argon2id login is nearer 200ms than 50ms; acceptable for staging, not for prod. |
| `api_min_capacity` / `api_max_capacity` | `2` / `6` | Autoscaling bounds for the API | `1` is cheaper and is the setting that costs the most: ADR 0005's Redis fan-out has nothing to fan out to with one task. `api_max_capacity` × `database_max_conns` is the peak connection count against a db.t4g.micro, whose own `max_connections` is around 100. |
| `web_desired_count` | `1` | Fixed task count for the web tier | Not a cost choice. #69: refresh-token rotation is per-process, so two web tasks can race on the same browser's session and the API's reuse detection signs the user out. Raise only after #69 lands. |
| `container_insights` | `false` | ECS Container Insights | `true` publishes per-task custom metrics whose bill is comparable to the tasks themselves. The Observability standard's actual requirement is a Prometheus endpoint (#12). |
| `wait_for_steady_state` | `true` | Block apply until both services are stable | `false` makes `terraform apply` report success over a crash-looping service, which is the most common way a deployment looks fine and is not. |
| `secret_recovery_window_days` | `0` | Days a deleted secret stays recoverable, billing $0.40/month | `0` purges immediately, which is right for an environment meant to be destroyed. Wrong for prod, where a deleted secret is usually a mistake. |
| `ecr_force_delete` / `alb_deletion_protection` | `true` / `false` | Destroyability | Both point the same way as `db_deletion_protection`: this environment must be destroyable, because leaving it running is the cost. |

**`nat_gateway_count = 0` is no longer a supported setting.** #101 offered it as
the one-variable way to shed half the bill. It now fails at plan time, twice
over: without egress a Fargate task cannot pull its image, and without a NAT
gateway there is no egress address, so `api_ingress_cidrs` is empty and
`modules/security-groups` rejects it. `terraform destroy` is the replacement.

### 1.1 Conventions invented here that later issues must honour

None of these is validated by anything; all of them propagate.

- **ECR repository names are slash-separated.** The execution role's pull
  permission is scoped to `repository/collabboard/*`, so #103 must create
  `collabboard/api`, not `collabboard-api`. A mismatch denies the pull and
  surfaces as `CannotPullContainerError` — indistinguishable at a glance from
  the missing-NAT case above.
- **Secrets live under `collabboard/<environment>/`.** The execution role can
  read anything under that prefix and nothing outside it, so #56's secrets must
  be named `collabboard/staging/db/app` and `collabboard/staging/db/owner` or
  the IAM policy needs widening too.

Invented by #102:

- **The two database secrets hold JSON with a `password` key**, matching the
  shape RDS itself produces for a managed credential. The task definitions read
  them as `<arn>:password::`. A secret whose value is a bare password string
  resolves to nothing and the task fails to start. #56 must write
  `{"username": "...", "password": "..."}`.
- **#102 creates those two secrets empty; #56 writes the values.** This is a
  change from what #101 anticipated. A `secrets:` entry needs an ARN at plan
  time, so the container has to exist before the task definition can name it. #56
  therefore adds versions, not secrets.
- **`AUTH_JWT_SECRET` lives at `collabboard/<environment>/auth/jwt`** as a plain
  string, and is written by an operator rather than by #56. Nothing tracked it
  before #102; the API refuses to start without it outside development.
- **The one-shot task definitions are `<prefix>-api-migrate` and
  `<prefix>-api-provision`.** #103 runs them with `aws ecs run-task`, in that
  order, before rolling the service.
- **Terraform owns the task definition's shape; #103 owns the image tag.** Both
  services set `ignore_changes = [task_definition]`, so a shape change made here
  registers a new revision that does *not* roll out on its own. #103's contract
  is to derive its revision from Terraform's latest — `describe-task-definition`,
  replace the image, `register-task-definition`, `update-service` — rather than
  building one from scratch, or every apply and every deploy will fight.
- **Task definitions are X86_64.** ARM64 is roughly 20% cheaper and the rest of
  this environment already runs Graviton, but an amd64 image on an ARM64 task
  definition fails at start with an exec format error. Changing it is a
  coordinated change with #103.
- **The web tier gets no task role at all.** Not an empty role — no
  `taskRoleArn`, so the Next.js process has no AWS credentials. Anything added
  there later is a new decision about which tier holds a credential.

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

### 2.2.1 What #102 changed about them

The secrets now exist as empty containers created by Terraform, with ARNs in the
`database_app_secret_arn` and `database_owner_secret_arn` outputs. What #56 has
left to do is write the values and run `api provision`. A task started before
that happens fails with `ResourceInitializationError` naming the secret — an
accurate way to say "nobody has provisioned this environment yet".

Writing one, once the value has been generated:

```
aws secretsmanager put-secret-value \
  --secret-id "$(terraform output -raw database_app_secret_arn)" \
  --secret-string "{\"username\":\"collabboard_app\",\"password\":\"$PASSWORD\"}"
```

### 2.3 AUTH_JWT_SECRET

| | |
|---|---|
| **Where** | Secrets Manager, `collabboard/<environment>/auth/jwt`. ARN is the `jwt_secret_arn` output. Created empty by #102. |
| **Written by** | A **human operator**, once. Deliberately not Terraform: a `random_password` resource would put the signing key in Terraform state, which is the one thing this stack has been careful never to do. |
| **Read by** | The API execution role, which resolves it into the container's environment before the process starts. Also present in the migrate and provision task definitions, which never use it — `config.Load` runs before the subcommand is dispatched and refuses to return without one. |
| **Rotation** | Rotating it invalidates every access token in flight, so every signed-in user is bounced to a refresh. Change the secret, then roll the tasks. |
| **Generating one** | `openssl rand -hex 32` — 64 hex characters, comfortably over the 32-byte floor `config.Load` enforces. |
| **To write it** | `aws secretsmanager put-secret-value --secret-id "$(terraform output -raw jwt_secret_arn)" --secret-string "$(openssl rand -hex 32)"` |

### 2.4 Terraform state

Not a named secret, but it holds resource attributes in plaintext including ones
redacted in plan output. Treat the state bucket as a credential store.

- **Environment state**: `s3://collabboard-tfstate-<account-id>/staging/base/terraform.tfstate`, versioned, encrypted with `alias/collabboard-tfstate`.
- **Who can read it**: anyone with S3 read on the bucket *and* `kms:Decrypt` on that key. Today the key policy is AWS's default, which delegates to IAM — so in practice, any admin identity in the account. Narrowing this is called out in ADR 0011 and belongs with #103's OIDC role.
- **Bootstrap state is local and gitignored.** It describes a bucket, a KMS key and an alias — no credential — so losing it costs an import, not a leak. That asymmetry is why bootstrap is a separate stack.

### 2.5 The operator's own AWS credentials

Never in this repository, never in a tfvars. A named CLI profile or SSO session.
`aws sts get-caller-identity` is the check that one is active.

---

## 3. What Terraform cannot do

Four things, three of them chicken-and-egg.

1. **An AWS account and an identity that can create IAM roles, KMS keys, VPCs, RDS and S3.** Must exist before the first `init`. Nothing in this repo creates it.
2. **The bootstrap apply.** `bootstrap/` creates the S3 bucket that every environment stores state in, so it cannot store its own state there. It runs first, with local state.
3. **Filling in `environments/staging/backend.hcl`.** The bucket name contains the AWS account ID and the KMS key ARN contains a generated UUID, and a `backend` block accepts no variables, functions or references — so neither can be computed and both must be pasted. `bootstrap`'s `backend_config` output prints the exact contents. **`kms_key_id` must be the key ARN, not the `alias/...` name**: the S3 backend rejects an alias, and the state bucket's `DenyWrongKmsKey` policy compares the request header against the exact ARN, so an alias would 403 every state write *and* every `.tflock` acquisition — a permanent lockout whose error message says "Access Denied" and points at IAM.
4. **A container image for each service.** Nothing in this repository builds one. `apps/web` has a Dockerfile; **`apps/api` has none at all**, and neither is built or pushed by CI yet — that is #103. Both services name `<repo>:bootstrap` by default, and with `wait_for_steady_state = true` an apply against an empty registry fails after roughly ten minutes.
5. **Writing the three secret values.** Terraform creates the containers and nothing else. Two are #56's; `auth/jwt` is the operator's.
6. **Running `bootstrap-owner.sql`, `api migrate up` and `api provision`.** #102 provides the task definitions that make all three possible — see ADR 0013 and section 4 below — but starting them is an operator or pipeline action, not an apply.
7. **Confirming the SNS subscription.** Terraform can create an email subscription but cannot click the confirmation link, so a subscription resource would sit permanently pending and look configured while notifying nobody. The topic is created; subscribing is step 15.

#101's version of this list said #56 was blocked on #102 rather than on #101. **#102 unblocks it**: the `<prefix>-admin` task definition is a psql session inside the VPC, which is the network path that did not previously exist.

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

### 4.1 #102: the load balancer, the services, and the first SQL

Steps 10 onwards assume 0-9 are done. **The order matters more here than it did
in #101**: several of these fail in ways that read like a different problem.

| # | Do | Verify |
|---|---|---|
| 10 | Replace the three placeholders in `terraform.tfvars` — `web_hostname`, `api_hostname`, `route53_zone_id` | `grep -c example.com environments/staging/terraform.tfvars` returns 0, and `aws route53 get-hosted-zone --id <zone>` prints the zone whose name both hostnames end in |
| 11 | `terraform apply -target=module.ecs.aws_ecr_repository.api -target=module.ecs.aws_ecr_repository.web` | `aws ecr describe-repositories --repository-names collabboard/api collabboard/web` succeeds. Targeting is normally a smell; here it is the only way out of the chicken-and-egg, because the services will not start without an image and the repositories do not exist until an apply |
| 12 | Build and push an image to each repository tagged `bootstrap` (`api_image_tag`/`web_image_tag`) | `aws ecr describe-images --repository-name collabboard/api --image-ids imageTag=bootstrap` returns a manifest. **`apps/api` has no Dockerfile yet** — see the blocking issues in the #102 PR |
| 13 | Write `AUTH_JWT_SECRET`: `aws secretsmanager put-secret-value --secret-id "$(terraform output -raw jwt_secret_arn)" --secret-string "$(openssl rand -hex 32)"` | `aws secretsmanager get-secret-value --secret-id ... --query 'length(SecretString)'` returns 64 |
| 14 | `terraform apply` | Certificate validation is the long pole after RDS: `aws acm describe-certificate --certificate-arn "$(terraform output -raw ...)"` reaches `ISSUED`. Expect apply to **fail at the services** until #56 has run — that is step 18 |
| 15 | Subscribe to the alarm topic: `aws sns subscribe --topic-arn "$(terraform output -raw alarm_topic_arn)" --protocol email --notification-endpoint you@example.com`, then click the link | `aws sns list-subscriptions-by-topic --topic-arn ...` shows a subscription whose ARN is not `PendingConfirmation` |
| 16 | Confirm `/healthz` and `/metrics` are not public: `curl -s -o /dev/null -w '%{http_code}' https://<web_hostname>/healthz` and `.../metrics` | Both `404`. Then confirm the health check still works: `aws elbv2 describe-target-health --target-group-arn ...` shows `healthy` targets. These two answers together are the whole point of the listener rules |
| 17 | Confirm the API is not public. From outside the VPC: `curl -m 10 https://<api_hostname>:8443/healthz` | **Times out.** A connection refused or a 404 would mean the security group is wrong. It is reachable only from this environment's NAT address |
| 18 | **#56.** Start the administrative task, exec in, run `bootstrap-owner.sql` | See 4.2 below |
| 19 | `aws ecs run-task --task-definition "$(terraform output -raw migrate_task_definition_arn)" ...` | The task exits `0`. Its log is in the API log group under the `ecs` stream prefix. A non-zero exit here means **stop** — do not roll the service (ADR 0013) |
| 20 | Write the app-role secret, then `run-task` with `provision_task_definition_arn` | Exits `0` and logs `database role password set from configuration` |
| 21 | `terraform apply` again, now that the services can actually start | Both services reach steady state; apply returns instead of timing out |
| 22 | End to end: open `https://<web_hostname>`, register, create a board, open it in two browsers and move a card | The card moves in the other window. **This is the only check that proves the WebSocket survived the load balancer** — everything above can be green while the idle timeout quietly kills the stream |

Step 22 is the one that cannot be replaced by a CLI command, and it is the one
this whole issue is about. If it fails, check `terraform output
alb_idle_timeout_seconds` against `REALTIME_PING_INTERVAL` first.

### 4.2 Running one-shot SQL against the database (step 18, and break-glass)

There is no bastion and no VPN. The path is a one-off Fargate task with ECS Exec;
ADR 0013 is why.

```
CLUSTER=$(terraform output -raw ecs_cluster_name)
TASKDEF=$(terraform output -raw admin_task_definition_arn)
NETCFG=$(terraform output -raw admin_run_task_network_configuration)

# 1. The master password, read with YOUR OWN IAM identity. No role in the ECS
#    layer can read this -- including the task you are about to start.
aws secretsmanager get-secret-value \
  --secret-id "$(terraform output -raw database_master_user_secret_arn)" \
  --query SecretString --output text

# 2. Start the task. --enable-execute-command is a run-task flag, not something
#    the task definition can carry.
TASK=$(aws ecs run-task --cluster "$CLUSTER" --task-definition "$TASKDEF" \
  --launch-type FARGATE --enable-execute-command \
  --network-configuration "$NETCFG" \
  --query 'tasks[0].taskArn' --output text)

aws ecs wait tasks-running --cluster "$CLUSTER" --tasks "$TASK"

# 3. Shell in.
aws ecs execute-command --cluster "$CLUSTER" --task "$TASK" \
  --container admin --interactive --command /bin/sh
```

Inside the session, `PGHOST`, `PGPORT`, `PGDATABASE` and `PGSSLMODE` are already
set. Paste `bootstrap-owner.sql` with a heredoc and run it:

```
cat > /tmp/bootstrap.sql <<'SQL'
... contents of apps/api/scripts/provision/bootstrap-owner.sql ...
SQL

psql -U collabboard_master -W -v ON_ERROR_STOP=1 \
     -v owner_password="<the owner password you generated>" \
     -f /tmp/bootstrap.sql
```

Three things about this that are easy to get wrong:

- **`-W`, always.** It prompts without echoing. **ECS Exec sessions are
  transcribed to `/ecs/<prefix>/exec` and kept for a year**, so a password on a
  command line or in a `PGPASSWORD=` assignment is a credential written to
  CloudWatch Logs. That retention is deliberate — it is the audit trail for every
  break-glass session against the database — which is exactly why nothing secret
  should be visible in one.
- **The task stops itself after an hour** (`sleep 3600`). Run it again for
  another hour. A break-glass path that has to be cleaned up by hand is one that
  gets left running.
- **`aws ecs execute-command` failing with a timeout rather than a permissions
  error** almost always means egress: the SSM messages channel needs the NAT
  gateway.

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
| The ACM certificate | No | Deleted with the listeners. The Route 53 records go with it; the hosted zone itself was never Terraform's. |
| `/ecs/<prefix>/exec` and `/ecs/<prefix>/admin` | **Yes**, per GB stored | Terraform-managed, so `destroy` removes them — but they hold the break-glass audit trail at 365-day retention, which is usually the thing you least want to lose in a teardown. Export before destroying if the environment ever had a real incident. |
| ECR images | **Yes**, $0.10/GB-month | `ecr_force_delete = true` in staging deletes the repository with its images. With it `false`, `destroy` fails on a non-empty repository. |
| The SNS topic and its subscription | No | Removed with the stack. The confirmation email is not reusable, so recreating the environment means subscribing again. |

Not a problem, worth knowing: automated RDS backups are removed
(`delete_automated_backups = true`), the attachments bucket deletes with objects
still in it (`attachments_force_destroy = true` in staging — this would fail in
prod), and the Elastic IP is released with its NAT gateway rather than lingering
as an unattached address, which is the classic surprise charge.

**That one-variable escape hatch is gone as of #102.** `nat_gateway_count = 0`
now fails at plan time — no egress means no image pull, and no egress address
means an empty `api_ingress_cidrs`. The environment costs roughly $106/month idle
and the way to stop paying for it is `terraform destroy`, which staging is
configured to make safe: no deletion protection anywhere, `skip_final_snapshot`,
`force_destroy` on the bucket and the repositories, and a zero-day secret
recovery window.
