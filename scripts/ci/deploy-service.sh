#!/usr/bin/env bash
#
# Roll one ECS service onto a new image, and wait until it is actually serving.
#
# Not `aws ecs update-service --force-new-deployment`: that redeploys the SAME
# task definition, which is a restart, not a deploy. The image lives in the task
# definition, so a new image means a new revision.
#
# Not `amazon-ecs-deploy-task-definition` either, for one specific reason: that
# action renders a task definition from a JSON file committed to the repository,
# which makes the repository the source of truth for a document Terraform also
# owns. The two drift the first time a Terraform change touches the task
# definition, and the drift is silent -- the pipeline keeps deploying the
# repository's version and the plan keeps showing no change. Reading the live
# definition and changing only the image keeps Terraform authoritative over
# everything except the one field a deploy is entitled to change.
#
# Required environment: CLUSTER, SERVICE, CONTAINER, IMAGE.

set -euo pipefail

: "${CLUSTER:?CLUSTER is required}"
: "${SERVICE:?SERVICE is required}"
: "${CONTAINER:?CONTAINER is required}"
: "${IMAGE:?IMAGE is required}"

echo "::group::Register a new revision of ${SERVICE} with ${IMAGE}"

# The definition the service is running right now, not `family:latest`. If a
# previous deploy registered a revision the service was never updated to --
# because the update failed after the register succeeded -- `latest` would build
# the next revision on top of a revision nobody is running.
current_arn="$(aws ecs describe-services \
  --cluster "$CLUSTER" \
  --services "$SERVICE" \
  --query 'services[0].taskDefinition' \
  --output text)"

if [ -z "$current_arn" ] || [ "$current_arn" = "None" ]; then
  echo "::error title=No such service::${SERVICE} not found in cluster ${CLUSTER}."
  exit 1
fi

echo "current task definition: ${current_arn}"

# Strip the fields DescribeTaskDefinition returns and RegisterTaskDefinition
# rejects. Passing them back is the classic failure here and the error names the
# parameter rather than the cause.
aws ecs describe-task-definition \
  --task-definition "$current_arn" \
  --query 'taskDefinition' \
  --output json > /tmp/current-task-definition.json

jq --arg image "$IMAGE" --arg container "$CONTAINER" '
  # Fail loudly if the named container is not in this definition, rather than
  # registering an identical revision and reporting success. A typo in
  # CONTAINER would otherwise deploy nothing, twice a day, forever.
  if ([.containerDefinitions[] | select(.name == $container)] | length) == 0 then
    error("no container named \($container) in this task definition")
  else . end
  | .containerDefinitions |= map(if .name == $container then .image = $image else . end)
  | del(
      .taskDefinitionArn,
      .revision,
      .status,
      .requiresAttributes,
      .compatibilities,
      .registeredAt,
      .registeredBy,
      .deregisteredAt
    )
' /tmp/current-task-definition.json > /tmp/new-task-definition.json

new_arn="$(aws ecs register-task-definition \
  --cli-input-json file:///tmp/new-task-definition.json \
  --query 'taskDefinition.taskDefinitionArn' \
  --output text)"

echo "registered: ${new_arn}"
echo '::endgroup::'

echo "::group::Roll ${SERVICE}"
aws ecs update-service \
  --cluster "$CLUSTER" \
  --service "$SERVICE" \
  --task-definition "$new_arn" \
  --query 'service.deployments[0].{status:status,desired:desiredCount}' \
  --output json

# The step that makes a deploy a deploy rather than a request. Without it the
# job goes green the instant the API call returns, which is before a single new
# task has started, so a crash-looping image reports a successful deployment.
#
# ECS's own waiter is 15 minutes at 15-second intervals, which is longer than
# this job's timeout -- the job timeout is the real bound and it fails the run
# rather than passing it.
echo 'waiting for the service to reach steady state...'
aws ecs wait services-stable --cluster "$CLUSTER" --services "$SERVICE"

echo "${SERVICE} is stable on ${new_arn}"
echo '::endgroup::'
