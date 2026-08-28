variable "name_prefix" {
  description = "Prefix for every resource name, e.g. `collabboard-staging`."
  type        = string
}

variable "allowed_subjects" {
  description = <<-EOT
    The `sub` claims allowed to assume the deploy role, matched with StringLike.

    This is the entire access-control boundary of the role -- there is no
    network path, no password and no key, only this list. Every entry must
    name a repository AND constrain what within it, because a GitHub Actions
    token's `sub` is the only thing distinguishing this repository's `main`
    from any workflow anywhere on GitHub.

    The two shapes that are safe here:

      repo:OWNER/NAME:ref:refs/heads/main       a push to main
      repo:OWNER/NAME:environment:production    a job in a protected environment

    The `environment:` form is the stronger of the two, because GitHub will not
    mint a token carrying it until the environment's protection rules -- the
    manual approval -- have been satisfied. That is what makes the prod
    promotion gate real rather than decorative: the approval is enforced by the
    identity provider, not by an `if:` in a workflow somebody can edit.
  EOT
  type        = list(string)

  validation {
    condition     = length(var.allowed_subjects) > 0
    error_message = "allowed_subjects must not be empty: a trust policy with no sub condition is assumable by every repository on GitHub."
  }

  validation {
    condition     = alltrue([for s in var.allowed_subjects : startswith(s, "repo:")])
    error_message = "every allowed_subjects entry must start with `repo:` -- an entry that does not name a repository does not constrain who may assume this role."
  }

  # The one that matters. `repo:owner/name:*` reads like "this repository" and
  # is not: it matches `pull_request` and `pull_request_target` runs, which on
  # a public repository means anyone who opens a pull request can assume the
  # deploy role. That is precisely the fork-PR credential-exfiltration route
  # #103 asked to make impossible.
  validation {
    condition = alltrue([
      for s in var.allowed_subjects :
      !endswith(s, ":*") && !strcontains(s, ":pull_request")
    ])
    error_message = "an allowed_subjects entry must not end in `:*` or name pull_request. `repo:owner/name:*` matches a pull_request run, so any fork that opens a PR could assume this role. Constrain to a ref or an environment."
  }
}

variable "cluster_name" {
  description = "ECS cluster the deploy role may update services and run tasks in. Also the value of the `ecs:cluster` condition, so a task definition registered here cannot be run on another cluster."
  type        = string
}

variable "service_names" {
  description = "ECS services the deploy role may update. Enumerated rather than wildcarded, so a new service is a deliberate grant."
  type        = list(string)
}

variable "passable_role_arns" {
  description = <<-EOT
    Roles the deploy identity may name in a task definition.

    Enumerate them. `iam:PassRole` on `*` together with RegisterTaskDefinition
    is full account compromise -- register a definition naming any role in the
    account, run it, read whatever that role can read -- and it is the single
    most common way an ECS deploy policy is wrong.
  EOT
  type        = list(string)

  validation {
    condition     = !contains(var.passable_role_arns, "*")
    error_message = "passable_role_arns must not contain `*`. PassRole on every role, combined with RegisterTaskDefinition, is unrestricted privilege escalation in this account."
  }
}

variable "ecr_repository_prefix" {
  description = "First path component of the repositories the deploy role may push to. Must match `ecr_namespace` in modules/ecs."
  type        = string
}

variable "log_group_prefix" {
  description = "Prefix of the CloudWatch log groups the deploy role may read, so a failed migration's reason can reach the pipeline's output. Read-only: deleting a log is how a bad deploy erases its own evidence."
  type        = string
}

variable "denied_secret_arns" {
  description = "Secret ARNs this role is explicitly denied, ADR 0001. Today the role has no secretsmanager Allow at all, so the Deny changes nothing -- which is the point: it cannot be overridden by an Allow attached later."
  type        = list(string)

  validation {
    condition     = length(var.denied_secret_arns) > 0
    error_message = "denied_secret_arns must not be empty. An empty list produces a Deny statement with no resources, which is valid IAM that denies nothing -- the failure mode modules/iam already had to fix once."
  }
}
