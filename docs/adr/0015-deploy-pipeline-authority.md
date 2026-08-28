# 0015. What the deploy pipeline is allowed to change

Date: 2026-08-28
Status: accepted

## Context

#103 asked for merge-to-`main` to deploy staging automatically, with a manual
gate before prod. That leaves one question the issue did not settle and which is
expensive to get wrong in either direction: **does the pipeline run `terraform
apply`?**

It is a real fork, not a detail. A pipeline that applies Terraform is a pipeline
whose credential can create IAM roles, change security groups and delete
databases; one that does not needs an identity that can push an image and roll a
service, and nothing else. The two roles differ by roughly two orders of
magnitude in what a leaked session can do, and the choice also decides who is
responsible for a bad plan at 2am.

Three further constraints came from what already exists:

- **Nothing has ever been applied.** There are no AWS credentials on the
  development machine and none in CI, so the first apply will be a human at a
  terminal reading a plan. A pipeline that applies would have to be trusted
  before it had ever been observed working.
- **ADR 0011 keeps state in S3 with native locking**, so concurrent applies are
  safe from corruption but not from surprise.
- **ADR 0013 fixed the order** in which schema changes reach a deployed
  database: `api migrate up` runs, and if it fails the service is not rolled.
  Whatever deploys has to honour that rather than re-deciding it.

## Decision

**The pipeline deploys images. It does not change infrastructure.**

Concretely, the GitHub Actions deploy role may:

- push to this project's two ECR repositories
- register a task definition and update the two services
- run the one-shot migration task in this cluster
- read the deploy log groups

and may not create, modify or delete any other AWS resource. It holds no
`secretsmanager`, `kms`, `rds`, `ec2`, `elasticloadbalancing` or `s3` grant at
all, and `iam:PassRole` — enumerated to three role ARNs and conditioned on
`ecs-tasks.amazonaws.com` — is the only `iam` action it carries. `terraform
apply` remains something a person runs, having read a plan.

The role is assumed through GitHub's OIDC provider. No long-lived AWS access key
exists for it, in a repository secret or anywhere else.

A deploy changes exactly one field of a task definition: the container image.
The definition is read live with `DescribeTaskDefinition`, the image is
substituted, and the result is registered as a new revision.

## Consequences

**A change that needs both new infrastructure and new code is two steps, in
order: apply, then merge.** That is friction, and it is the honest kind — it
matches the fact that one of those steps needs a human reading a plan and the
other does not. It also means a rollback of application code cannot accidentally
roll back infrastructure.

**"What is running" stays answerable.** Images are tagged with the full
40-character commit SHA and the ECR repositories are `IMMUTABLE`, so a tag names
one artifact forever. Rollback is therefore `workflow_dispatch` with a previous
SHA — a real procedure rather than an aspiration, and the reason the immutable
tag is not cosmetic.

**Terraform and the pipeline both touch task definitions, and they must not
fight.** The ECS services already ignore changes to their task definition, so
the pipeline rolling a new revision does not produce a permanent diff in the
next plan. Terraform stays authoritative over everything in the definition
except the image; the pipeline is authoritative over the image and nothing else.
This is why the deploy reads the live definition rather than rendering one from
a JSON file in the repository — a committed rendering is a second source of
truth for a document Terraform owns, and the two drift silently the first time
an infrastructure change touches it.

**The blast radius of a leaked deploy session is bounded but not zero, and the
bound should be stated rather than implied.** `RegisterTaskDefinition` plus
`PassRole` plus `RunTask` is enough to run an arbitrary image as the execution
role, and the execution role can read this environment's secrets. No ECS deploy
identity can avoid this; it is what deploying to ECS *is*. What limits it is
that the role is unassumable except by a workflow running from this repository
on `refs/heads/main` — the same boundary that protects `main` itself — and that
the session lasts an hour. It is not limited by the policy, and a reader who
works that out from the policy should not have to wonder whether anybody else
did.

**Prod promotion is gated by the identity provider, not by the workflow file.**
The prod deploy role will trust a `sub` of
`repo:OWNER/NAME:environment:production`, and GitHub does not mint a token
carrying that subject until the environment's required-reviewer rule is
satisfied. An `if:` in a workflow can be edited by whoever can open a PR; this
cannot be edited from inside the repository at all.

## Alternatives considered

**Pipeline runs `terraform apply` on merge.** The full "GitOps" shape, and the
one that reads best on a slide. Rejected for now on two grounds. The credential
it needs is close to account-administrator — which is a large thing to hand to a
workflow that has never been observed working, in an account that has never been
applied to. And a plan for this stack includes RDS and ElastiCache, where an
unreviewed diff can mean replacement, and replacement means data loss; `plan`
followed by a human reading it is not ceremony there. Worth revisiting once the
stack has been applied by hand a few times and the diffs are boring.

**Pipeline runs `terraform plan` on a PR and comments the diff, human applies.**
Genuinely attractive and deliberately deferred rather than rejected: it needs
only a read-only role, and it would catch exactly the class of mistake `fmt` and
`validate` cannot. It is not here because it needs AWS credentials to exist
first, which is the thing this whole sequence of work is building towards. Worth
filing once staging is applied.

**`aws-actions/amazon-ecs-deploy-task-definition` with a committed task
definition JSON.** The documented path, and rejected for the drift described
above: it makes the repository the source of truth for a document Terraform
owns, and the divergence is silent in both directions.

**`update-service --force-new-deployment` against a mutable `latest` tag.** One
line instead of a script. Rejected because it makes "which commit is running"
unanswerable, which is the question that matters during an incident and the only
time nobody has the patience to reconstruct it.

## References

- #103, and `Standards/CI-CD Standards.md` stages 6–9
- ADR 0011 (remote state), ADR 0013 (migration ordering)
- `infra/terraform/modules/cicd/` — the role, and the tests asserting these bounds
- `.github/workflows/cd.yml`, `scripts/ci/`
