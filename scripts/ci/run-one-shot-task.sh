#!/usr/bin/env bash
#
# Run a one-shot ECS task to completion and fail the job if it does not exit 0.
#
# Used for `api migrate up`. ADR 0013's ordering says a failed migration must
# stop the deploy rather than be rolled over, and this is where that is
# enforced: the exit code of the container becomes the exit code of the step.
#
# The task definition already carries the command, the roles and the secrets --
# Terraform owns it. This overrides one thing, the image, so the migration runs
# the same binary the deploy is about to roll out. A migration run from a
# different image than the service is a migration that proves nothing.
#
# Required environment: CLUSTER, TASK_DEFINITION, NETWORK_CONFIGURATION, IMAGE.

set -euo pipefail

: "${CLUSTER:?CLUSTER is required}"
: "${TASK_DEFINITION:?TASK_DEFINITION is required}"
: "${NETWORK_CONFIGURATION:?NETWORK_CONFIGURATION is required}"
: "${IMAGE:?IMAGE is required}"

echo "::group::Start ${TASK_DEFINITION}"

task_arn="$(aws ecs run-task \
  --cluster "$CLUSTER" \
  --task-definition "$TASK_DEFINITION" \
  --launch-type FARGATE \
  --network-configuration "$NETWORK_CONFIGURATION" \
  --overrides "$(jq -nc --arg image "$IMAGE" '{containerOverrides: [{name: "api", image: $image}]}')" \
  --started-by "cd-${GITHUB_RUN_ID:-manual}" \
  --query 'tasks[0].taskArn' \
  --output text)"

if [ -z "$task_arn" ] || [ "$task_arn" = "None" ]; then
  echo '::error title=Task did not start::run-task returned no task ARN. Check the failures array above.'
  exit 1
fi

echo "task: ${task_arn}"
echo '::endgroup::'

echo 'waiting for the task to stop...'
aws ecs wait tasks-stopped --cluster "$CLUSTER" --tasks "$task_arn"

# The exit code of the container, not of the task. A task can stop for reasons
# that never ran the container at all -- a secret with no version, an image that
# cannot be pulled -- and those report a null exit code, which must not be read
# as success.
read -r exit_code stopped_reason < <(
  aws ecs describe-tasks \
    --cluster "$CLUSTER" \
    --tasks "$task_arn" \
    --query 'tasks[0].[containers[0].exitCode, stoppedReason]' \
    --output text
)

echo "exit code: ${exit_code}"
echo "stopped reason: ${stopped_reason}"

if [ "$exit_code" != "0" ]; then
  echo "::error title=Migration failed::${TASK_DEFINITION} exited ${exit_code}: ${stopped_reason}"
  echo 'The deploy stops here. Per ADR 0013 the service is NOT rolled onto a schema the migration did not finish.'
  echo "Logs: the API log group, stream prefix ecs, task ${task_arn##*/}"
  exit 1
fi

echo "${TASK_DEFINITION} completed successfully"
