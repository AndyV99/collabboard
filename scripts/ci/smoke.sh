#!/usr/bin/env bash
#
# Prove the deploy is serving, from outside, before calling it successful.
#
# A service reaching steady state means ECS is satisfied with the task. It does
# not mean the product works: the load balancer could be routing nowhere, the
# listener rules could be wrong, or -- the failure this repository has already
# reasoned about twice -- the idle timeout could be reaping live WebSockets. So
# this runs against the public hostname over the internet, not against a
# container from inside the VPC.
#
# Deliberately small. A smoke test that tries to be an end-to-end suite becomes
# a flaky gate that people learn to re-run, and #17 owns the real e2e coverage.
#
# Required environment: WEB_URL.

set -euo pipefail

: "${WEB_URL:?WEB_URL is required}"

fail() { echo "::error title=Smoke check failed::$1"; exit 1; }

echo "::group::Smoke check against ${WEB_URL}"

# 1. The app answers at all, over TLS, through the load balancer.
#    --retry covers the window between "steady state" and the target group
#    finishing its own health checks; without it this is a race that fails
#    roughly one deploy in ten for no real reason.
status="$(curl -sS -o /dev/null -w '%{http_code}' \
  --retry 6 --retry-delay 5 --retry-all-errors --max-time 20 \
  "${WEB_URL}/")" || fail "could not reach ${WEB_URL}/"

case "$status" in
  200|307|308) echo "GET / -> ${status}" ;;
  *) fail "GET / returned ${status}, expected 200 or a redirect" ;;
esac

# 2. /healthz is NOT public. modules/alb answers it with a fixed 404 from the
#    internet while the target group still health-checks it internally, and
#    those two facts together are the whole point of those listener rules. A
#    200 here means the rule stopped working and the dependency detail in
#    /healthz's body is now on the internet (#31).
health_status="$(curl -sS -o /dev/null -w '%{http_code}' \
  --max-time 20 "${WEB_URL}/healthz")" || fail "could not reach ${WEB_URL}/healthz"

if [ "$health_status" != "404" ]; then
  fail "GET /healthz returned ${health_status}, expected 404 -- the listener rule that keeps it off the internet is not working"
fi
echo "GET /healthz -> 404 (correctly not public)"

# 3. Same for /metrics.
metrics_status="$(curl -sS -o /dev/null -w '%{http_code}' \
  --max-time 20 "${WEB_URL}/metrics")" || fail "could not reach ${WEB_URL}/metrics"

if [ "$metrics_status" != "404" ]; then
  fail "GET /metrics returned ${metrics_status}, expected 404"
fi
echo "GET /metrics -> 404 (correctly not public)"

echo '::endgroup::'
echo 'Smoke check passed.'
