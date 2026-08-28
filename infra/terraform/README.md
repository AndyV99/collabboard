# CollabBoard infrastructure

Terraform for the AWS network and data layer. The ECS service and ALB are #102,
the deploy pipeline is #103; neither is here.

**Nothing in this directory has been applied to a real AWS account.** It is
`fmt`-clean and `validate`-clean against AWS provider 6.58.0, and the module
unit tests pass against a mocked provider — CI enforces all three — but no
`plan` has run against a real account, because that needs credentials. Treat the
first apply as unproven.

```
bootstrap/              state backend: S3 bucket + KMS key. Run once, local state.
modules/
  network/              VPC, three subnet tiers per AZ, NAT, S3 gateway endpoint
  security-groups/      the whole "who can reach Postgres and Redis" story
  database/             RDS Postgres 16
  cache/                ElastiCache Redis 7
  storage/              S3 attachments bucket
  iam/                  ECS task + execution roles, API log group
environments/
  staging/              the only instantiated environment
OPERATOR-INPUTS.md      inventory of manual steps, secrets and teardown, for #105
```

Decisions worth reading before changing anything here:
`docs/adr/0011-terraform-remote-state-backend.md` (state layout, why one root
module per environment) and `docs/adr/0012-network-shape-and-cost.md` (subnet
tiers, and where the money goes).

## Bootstrap the state backend

Once per AWS account, before any environment can `init`. This stack creates the
bucket every other stack stores state in, so it keeps its own state locally —
that state holds no credential, only a bucket and a key.

```bash
cd infra/terraform/bootstrap
terraform init
terraform apply
terraform output -raw backend_config
```

Paste that output over the placeholder in
`environments/staging/backend.hcl`. It is two lines — a bucket name and a KMS
alias, neither of which is a secret. The bucket name contains the AWS account
ID and so cannot be committed ahead of time; a `backend` block accepts no
variables, which is why this is a paste rather than a variable.

## Plan an environment

```bash
cd infra/terraform/environments/staging
terraform init -backend-config=backend.hcl
terraform plan
```

`terraform.tfvars` is auto-loaded, so there is no `-var-file` to forget and no
way to plan this directory against another environment's values. Nothing is
prompted for.

Do not `apply` without reading `terraform.tfvars` first. It is where the cost
lives.

## What this costs

Roughly **$107/month** in us-east-1 at the committed staging settings, running
continuously. This table covers the whole stack — #101's base infrastructure,
#102's load balancer and services, and #103's registry.

| | |
|---|---|
| NAT gateway ×1 | $32.85 + $0.045/GB |
| Application Load Balancer | $16.43 + LCU, realistically **~$22** at low traffic |
| Fargate, 3 tasks × 0.25 vCPU / 0.5 GB, **ARM64** | $21.63 |
| RDS `db.t4g.micro`, single-AZ | $11.68 |
| ElastiCache `cache.t4g.micro` ×1 | $11.68 |
| RDS gp3 storage, 20 GB | $2.30, up to $5.75 if autoscaling fills |
| KMS customer-managed keys ×2 | $2.00 |
| Secrets Manager, 3 secrets | $1.20 |
| S3, ECR, CloudWatch Logs | ~$1.30 |

Two numbers in that table are estimates rather than list prices and should be
read as such. The **ALB's LCU charge** depends on connections, and this
application holds a WebSocket open per viewer, which is the dimension LCUs are
least forgiving about — $22 is a low-traffic guess, not a ceiling. **NAT data
processing** is $0.045/GB on everything the private subnets send or receive,
including every image pull.

### The levers, largest first

- **The NAT gateway is the single biggest line.** `nat_gateway_count = 0` used
  to drop $32.85 with no other change, and **no longer works**: the tasks live
  in private subnets and genuinely need egress to pull an image, and
  `modules/security-groups` rejects an empty NAT list at plan time rather than
  building a listener nothing can reach. Replacing it with VPC interface
  endpoints (ECR, S3, Secrets Manager, CloudWatch Logs) is the real alternative
  — roughly $14/month of endpoints against $33 of NAT — and is not done here.
- **ARM64 saves ~$5.41/month** against the same shape on X86_64 ($21.63 vs
  $27.04), which is where the arithmetic in #120 comes from. It is free in every
  other sense: the images build for it at the same speed and the rest of the
  environment was already Graviton.
- **`api_min_capacity = 1`** saves $7.21/month and costs something real: ADR
  0005's Redis fan-out never executes with one task, so the project's headline
  feature would run in a shape nothing has exercised.
- **Destroying the environment** takes it to approximately zero. See
  OPERATOR-INPUTS.md §5 for what survives `destroy` and keeps billing.

A faithful production shape — NAT per AZ, Multi-AZ RDS, a cache replica — is
about **$190/month** on top of the same compute.
`docs/adr/0012-network-shape-and-cost.md` sets out what each deviation gives up.

## Three things #102 must know

The first two are enforced by this configuration and both fail loudly, at
startup, rather than degrading:

- **ElastiCache requires TLS.** `transit_encryption_enabled = true`, so the API
  needs a non-nil `TLSConfig` for go-redis and Asynq. #112 made that reachable
  from configuration and #122 set it: `modules/ecs` writes
  `REDIS_TLS_ENABLED = "true"` into the **serve** task definition as a literal,
  matching the constant in `modules/cache` so the two cannot drift. The setting
  still defaults to `false` in the application, which is what the local compose
  stack needs — a `terraform test` case asserts the deployed value is `true`
  rather than merely present, because `false` would satisfy a presence check and
  fail the same way: a plaintext connection the listener refuses, `/healthz`
  answering 503, and an apply that times out on `wait_for_steady_state` about
  ten minutes later naming the service rather than the setting.

  `REDIS_HOST` must be the primary endpoint's **DNS name**, not an address: the
  client verifies the certificate against that name, and Go sends no SNI for an
  IP literal. `var.cache_host` is validated against the IPv4 shape, so that
  mistake is a plan-time error instead.
- **Postgres requires TLS.** `rds.force_ssl = 1`, so the DSN needs
  `sslmode=require` at minimum.

The third is a naming convention invented here that #102 and #103 have to
honour:

- **ECR repository names are slash-separated.** The execution role can pull from
  `repository/collabboard/*` — so `collabboard/api` works and `collabboard-api`
  does not. A mismatch denies the pull, and the symptom is
  `CannotPullContainerError`, which is the same symptom as a missing NAT
  gateway.

## Testing

```bash
cd infra/terraform/modules/network && terraform init && terraform test   # 8 cases
cd infra/terraform/modules/iam     && terraform init && terraform test   # 4 cases
```

CI runs `terraform test` for any module with a `tests/` directory, so adding
coverage needs no workflow change. The tests use `mock_provider`: no AWS
credentials, no API calls, no cost, so they run on a fork PR like everything
else.

**`modules/network`** covers what is pure computation and expensive to get
wrong — subnet CIDR derivation, and which NAT gateway each private route table
points at across all three `nat_gateway_count` settings, two of which `validate`
would never evaluate because staging instantiates only one. It also pins the
data tier's isolation, including the case that is invisible in the resource
graph: a VPC gateway endpoint injects a route without being an `aws_route`, so
associating the S3 endpoint with the data route table would silently give that
tier a path to every bucket in AWS.

**`modules/iam`** covers the invariant ADR 0001 rests on: the master credential
is Denied, the Deny names the right ARN and covers every `secretsmanager`
action, both roles carry it, and the task role holds no secret permission at
all. An empty or malformed `denied_secret_arns` is a plan-time error rather than
a silently absent control.

Both suites were mutation-tested rather than trusted — the S3-endpoint fault,
`Deny`→`Allow`, a dropped role attachment, and a secret grant added to the task
role were each reintroduced and each failed the intended assertion.

One limit worth knowing: a mocked provider computes
`aws_iam_policy_document.json`, so the IAM tests assert on the *configured*
statements rather than the rendered JSON. That catches a Deny that is missing,
misdirected, unattached or inverted; it cannot catch AWS evaluating a
well-formed policy differently than expected. `OPERATOR-INPUTS.md` step 8 closes
that gap with one `iam simulate-principal-policy` call against a real account.

## The constraint this configuration exists to protect

ADR 0001's tenant isolation depends on the API connecting as `collabboard_app`,
a role that is not a superuser, owns nothing, and cannot bypass row-level
security. RDS hands you a master user that is none of those things.

So the master password is generated and held by **RDS**, never by Terraform — it
is not in state, not in a plan, and not in this repository — and both ECS roles
carry an explicit IAM `Deny` on its secret. Wiring the application to the master
credential is not a shortcut available by accident; it requires deleting a Deny
statement, and `modules/iam/tests` fails if anyone does. Provisioning the real
roles is #56.
