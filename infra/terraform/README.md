# CollabBoard infrastructure

Terraform for the AWS network and data layer. The ECS service and ALB are #102,
the deploy pipeline is #103; neither is here.

**Nothing in this directory has been applied to a real AWS account.** It is
`fmt`-clean and `validate`-clean against AWS provider 6.58.0, and CI enforces
both, but no `plan` has run against a real account because that needs
credentials. Treat the first apply as unproven.

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

Roughly **$61/month** in us-east-1 at the committed staging settings, mostly
idle:

| | |
|---|---|
| NAT gateway ×1 | $32.85 + $0.045/GB |
| RDS `db.t4g.micro`, single-AZ | $11.68 |
| RDS gp3 storage, 20 GB | $2.30 |
| ElastiCache `cache.t4g.micro` ×1 | $11.68 |
| KMS customer-managed keys ×2 | $2.00 |
| S3, CloudWatch Logs | ~$1.00 |

The NAT gateway is over half of it and buys nothing until #102 exists — nothing
runs in the private subnets yet, and the data tier has no egress in any
configuration. Setting `nat_gateway_count = 0` drops this to about **$28/month**
with no other change. It must be back to at least `1` before the ECS service
lands, or tasks cannot pull their image.

A faithful production shape — NAT per AZ, Multi-AZ RDS, a cache replica — is
about **$140/month**. `docs/adr/0012-network-shape-and-cost.md` sets out what
each deviation gives up.

## Two things #102 must know

Both are enforced by this configuration and both fail loudly, at startup, rather
than degrading:

- **ElastiCache requires TLS.** `transit_encryption_enabled = true`, so the API
  needs `rediss://` or a non-nil `TLSConfig` for go-redis and Asynq.
- **Postgres requires TLS.** `rds.force_ssl = 1`, so the DSN needs
  `sslmode=require` at minimum.

## The constraint this configuration exists to protect

ADR 0001's tenant isolation depends on the API connecting as `collabboard_app`,
a role that is not a superuser, owns nothing, and cannot bypass row-level
security. RDS hands you a master user that is none of those things.

So the master password is generated and held by **RDS**, never by Terraform — it
is not in state, not in a plan, and not in this repository — and both ECS roles
carry an explicit IAM `Deny` on its secret. Wiring the application to the master
credential is not a shortcut available by accident; it requires deleting a Deny
statement. Provisioning the real roles is #56.
