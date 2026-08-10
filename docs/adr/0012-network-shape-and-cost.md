# 0012. Three-tier VPC, and buying fidelity back where it is cheap

Date: 2026-08-10
Status: accepted

## Context

The network shape is the most expensive infrastructure decision in this project
to reverse. Subnet CIDRs are fixed at VPC creation; moving an RDS instance
between subnet groups is a rebuild; and every issue after this one — #102's ECS
service and ALB, #103's deploy pipeline, #56's role provisioning — is built on
top of whatever this establishes.

It is also the decision where the textbook answer and the right answer diverge,
because this runs on a personal account and is paid for by one person. The
faithful production shape — two AZs, a NAT gateway in each, a Multi-AZ RDS
instance, a replicated cache — costs roughly **$140/month** for an environment
that will sit idle between demos. Two thirds of that is redundancy for failures
that, in a staging environment nobody depends on, are events to be repaired
rather than survived.

The trap in reacting to that is picking the cheap shape and describing it as
production-grade. The opposite trap is treating "cheap" as a single dial, when
in fact the bill is dominated by a handful of line items and most hardening
costs nothing at all.

## Decision

**Two AZs and three subnet tiers per AZ: `public` (internet gateway), `private`
(NAT egress, no ingress), and `data` (no default route in any direction).** RDS
and ElastiCache go in `data`. The staging environment then buys down the
redundancy line items specifically, and nothing else.

Staging deviates from the production shape in exactly four places, each a
variable rather than a hardcoded value:

| Knob | Staging | Production shape | Saved |
|---|---|---|---|
| `nat_gateway_count` | 1 | 2 | $32.85/mo |
| `db_multi_az` | false | true | $11.68/mo |
| `db_instance_class` | db.t4g.micro | db.t4g.small+ | $11.68/mo |
| `cache_node_count` | 1 | 2 | $11.68/mo |

Approximate monthly cost of what this stands up, us-east-1, 730 hours:

```
NAT gateway (1)                    $32.85   + $0.045/GB processed
RDS db.t4g.micro, single-AZ        $11.68
RDS gp3 storage, 20 GB              $2.30
ElastiCache cache.t4g.micro (1)    $11.68
KMS customer-managed keys (2)       $2.00   state + staging data
S3, DynamoDB, CloudWatch Logs      ~$1.00   usage-driven, negligible at idle
                                   -------
                                   ~$61/mo
```

**The NAT gateway is over half of it**, and it is the one line that buys nothing
until #102 exists — nothing runs in the private subnets yet, and the data tier
has no egress in any configuration. `nat_gateway_count = 0` is therefore a valid
setting that drops this environment to **~$28/month**, and it is documented in
`terraform.tfvars` rather than hidden, because "stood up for a demo and
forgotten" is the realistic failure mode for a portfolio environment. It must be
raised to at least 1 before #102, or Fargate tasks fail to pull their image.

Interface VPC endpoints for ECR, CloudWatch Logs and Secrets Manager were
considered as a NAT replacement and rejected on arithmetic: four of them at
$7.30/mo is $29.20, within pennies of the single NAT, while covering strictly
less (no Stripe, no OTLP export). The S3 *gateway* endpoint is free and is
included, which keeps attachment traffic off the NAT's per-GB charge.

**Hardening that costs nothing was not deviated on.** The data tier's isolation,
`rds.force_ssl = 1`, TLS-required ElastiCache, encryption at rest under a
customer-managed key, S3 public access blocked with ACLs disabled, TLS-only
bucket policies, and least-privilege IAM are all present at staging settings,
because none of them has a price.

## Consequences

**"The database cannot reach the internet" is a routing fact, not a security
group convention.** The third tier is the reason. A security group is a rule
somebody widens later during an incident; a route table with no `0.0.0.0/0`
entry, and no code path that adds one, is a different class of guarantee. It
cost two subnets and one route table — nothing recurring. Combined with
`publicly_accessible = false` and an ingress rule referencing only the app
security group, reaching Postgres from the internet requires three independent
mistakes.

**Two things will bite #102 if it is not expecting them, and both fail loudly
rather than silently.** ElastiCache has `transit_encryption_enabled = true`, so
the API must connect with TLS (`rediss://`, or a non-nil `TLSConfig` for
go-redis and Asynq); a plaintext client does not connect. Postgres has
`rds.force_ssl = 1`, so the DSN needs `sslmode=require` at minimum. Both are
exported as outputs specifically so the next issue cannot build a connection
string without meeting them.

**Redis has TLS but no AUTH token, deliberately.** An auth token is a second
credential with the same provisioning and rotation problem as the database
secrets in #56, and it defends against an attacker who already has a network
route to the node — which requires membership in the app security group. The
cost/benefit changes once #56 has established the secret-provisioning pattern,
and revisiting it then is the intent. Recording it here rather than leaving an
unexplained absence.

**A single NAT means egress has an AZ-shaped failure domain.** If `us-east-1a`
fails, tasks in `us-east-1b` lose outbound connectivity even though they are
running. Accepted because the database is single-AZ in this environment anyway:
the second NAT would be protecting the more resilient half of the system. Both
knobs move together when this becomes prod.

**No VPC flow logs.** They are the obvious next security control and they were
left out rather than added as an off-by-default variable, because a knob nobody
turns on is indistinguishable from a gap. The cost is CloudWatch ingest at
$0.50/GB, which is small but not nothing at idle. If network forensics becomes a
requirement, it should arrive as a decision with a destination (S3, not
CloudWatch Logs, for cost) rather than as a flag.

**CIDR allocation is invented and is the cheapest thing here to change today.**
`10.10.0.0/16` dev, `10.20.0.0/16` staging, `10.30.0.0/16` prod, non-overlapping
so a future peering or transit gateway has somewhere to go. Nothing outside this
repository depends on it yet.

**Reversal.** The knobs reverse trivially — `nat_gateway_count`, `db_multi_az`
and `cache_node_count` are one-line changes, and raising them is an in-place
modification (RDS Multi-AZ conversion is online; adding a cache replica is
online). `db_instance_class` is a short restart. The tier structure is the
expensive part: changing subnet CIDRs or collapsing the data tier means
recreating the VPC and every subnet-bound resource in it, which is a rebuild and
a data migration. That asymmetry is why the tiers are the production shape now
and the sizing is not.
