# The three properties of this module that are easy to break and expensive to
# notice, asserted rather than argued in a comment:
#
#   1. /healthz and /metrics are answered with a 404 by both HTTPS listeners,
#      while the target groups still health-check /healthz. Getting the second
#      half wrong makes every task unhealthy; getting the first half wrong
#      publishes an endpoint that names its dependencies and their errors.
#   2. The idle timeout cannot fall below the realtime hub's ping interval. That
#      failure is silent -- live boards go dead on a timer and nothing logs an
#      error -- so it is a plan-time rejection.
#   3. The API listener and the web listener are different ports, because the
#      security group is the only per-port control an ALB has and it is what
#      keeps the API off the internet.
#
# `mock_provider` means no credentials and no API calls. As with modules/iam,
# these assertions read *configured* values, which Terraform evaluates itself --
# they cannot tell you how AWS behaves, only that the configuration says what it
# is supposed to say.
#
#   cd infra/terraform/modules/alb && terraform init && terraform test

# `domain_validation_options` is computed, and main.tf uses it as a for_each
# collection. A mocked provider returns it unknown, and a for_each over an
# unknown value is an error -- so the certificate is overridden with a plausible
# pair of records. Nothing below asserts on them; they exist so the module can be
# evaluated offline.
mock_provider "aws" {
  override_resource {
    target = aws_acm_certificate.this
    values = {
      arn = "arn:aws:acm:us-east-1:000000000000:certificate/00000000-0000-0000-0000-000000000000"
      domain_validation_options = [
        {
          domain_name           = "staging.collabboard.test"
          resource_record_name  = "_acme.staging.collabboard.test."
          resource_record_type  = "CNAME"
          resource_record_value = "validation.acm-validations.aws."
        },
        {
          domain_name           = "api.staging.collabboard.test"
          resource_record_name  = "_acme.api.staging.collabboard.test."
          resource_record_type  = "CNAME"
          resource_record_value = "validation.acm-validations.aws."
        },
      ]
    }
  }

  # A mocked provider generates a random string for every computed attribute, and
  # several resources here validate that an ARN argument parses as an ARN. Same
  # reason as the deny-policy ARN override in modules/iam/tests: without these the
  # module cannot be evaluated offline at all.
  #
  # None of the assertions below reads any of these values. They are fixtures,
  # and asserting on a fixture proves only that the fixture was written
  # correctly -- the listener rules, ports, timeouts and health checks that ARE
  # asserted are all configured input, which Terraform evaluates itself.
  override_resource {
    target = aws_sns_topic.alarms
    values = {
      arn = "arn:aws:sns:us-east-1:000000000000:collabboard-test-alarms"
    }
  }

  override_resource {
    target = aws_lb.this
    values = {
      arn        = "arn:aws:elasticloadbalancing:us-east-1:000000000000:loadbalancer/app/collabboard-test/0000000000000000"
      arn_suffix = "app/collabboard-test/0000000000000000"
    }
  }

  override_resource {
    target = aws_lb_target_group.api
    values = {
      arn        = "arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/collabboard-test-api/0000000000000001"
      arn_suffix = "targetgroup/collabboard-test-api/0000000000000001"
    }
  }

  override_resource {
    target = aws_lb_target_group.web
    values = {
      arn        = "arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/collabboard-test-web/0000000000000002"
      arn_suffix = "targetgroup/collabboard-test-web/0000000000000002"
    }
  }

  override_resource {
    target = aws_lb_listener.web
    values = {
      arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:listener/app/collabboard-test/0000000000000000/0000000000000003"
    }
  }

  override_resource {
    target = aws_lb_listener.api
    values = {
      arn = "arn:aws:elasticloadbalancing:us-east-1:000000000000:listener/app/collabboard-test/0000000000000000/0000000000000004"
    }
  }
}

variables {
  name_prefix       = "collabboard-test"
  vpc_id            = "vpc-00000000000000000"
  public_subnet_ids = ["subnet-00000000000000001", "subnet-00000000000000002"]
  security_group_id = "sg-00000000000000000"

  web_hostname    = "staging.collabboard.test"
  api_hostname    = "api.staging.collabboard.test"
  route53_zone_id = "Z00000000000000000000"

  realtime_ping_interval_seconds = 25
  realtime_pong_timeout_seconds  = 10
}

# The distinction the whole issue turns on. A target group health check is issued
# by the load balancer straight to the target's address and port; it never
# traverses a listener, so it does not see the 404 rule. "Reachable by the health
# check" and "reachable from the internet" are different properties and this
# asserts both halves at once -- a change that satisfied one by breaking the
# other would pass a test that only checked one.
run "healthz_is_blocked_publicly_and_still_health_checked" {
  command = apply

  assert {
    condition     = contains(keys(aws_lb_listener_rule.web_blocked), "/healthz")
    error_message = "the public HTTPS listener must answer /healthz itself rather than forwarding it -- apps/api's /healthz names each dependency it probed and, per #31, the raw error from any that failed"
  }

  assert {
    condition     = contains(keys(aws_lb_listener_rule.api_blocked), "/healthz")
    error_message = "the API listener must block /healthz too; a control that only works while the address restriction also works is one control, not two"
  }

  assert {
    condition = alltrue([
      aws_lb_target_group.api.health_check[0].path == "/healthz",
      aws_lb_target_group.web.health_check[0].path == "/healthz",
    ])
    error_message = "both target groups must still health-check /healthz -- blocking the path at the listener must not blind the health check, or every task is permanently unhealthy"
  }
}

# /metrics does not exist yet (#12). That is exactly why the rule is here: when
# #12 lands, the endpoint arrives unreachable instead of arriving published, and
# publishing it becomes a visible edit rather than a default nobody noticed.
run "metrics_is_blocked_on_both_listeners" {
  command = apply

  assert {
    condition = alltrue([
      contains(keys(aws_lb_listener_rule.web_blocked), "/metrics"),
      contains(keys(aws_lb_listener_rule.api_blocked), "/metrics"),
    ])
    error_message = "/metrics must be blocked on both HTTPS listeners before #12 creates it"
  }

  # A prefix match as well as the exact path, or /metrics/foo walks straight
  # past the rule. `one()` rather than an index because a listener rule's
  # `condition` is a set, and a set has no index.
  assert {
    condition = alltrue([
      for path, rule in aws_lb_listener_rule.web_blocked :
      length(one(one(rule.condition).path_pattern).values) == 2 &&
      contains(one(one(rule.condition).path_pattern).values, path) &&
      contains(one(one(rule.condition).path_pattern).values, "${path}/*")
    ])
    error_message = "each blocked path must match both itself and everything under it -- an exact match alone lets /metrics/foo walk straight past the rule"
  }
}

run "blocked_paths_return_a_fixed_404_rather_than_forwarding" {
  command = apply

  assert {
    condition = alltrue([
      for rule in concat(values(aws_lb_listener_rule.web_blocked), values(aws_lb_listener_rule.api_blocked)) :
      rule.action[0].type == "fixed-response"
    ])
    error_message = "a blocked path must be answered by the load balancer, not forwarded to a target group"
  }

  # 404, not 403. A 403 confirms that something is there.
  assert {
    condition = alltrue([
      for rule in concat(values(aws_lb_listener_rule.web_blocked), values(aws_lb_listener_rule.api_blocked)) :
      rule.action[0].fixed_response[0].status_code == "404"
    ])
    error_message = "blocked paths must answer 404; 403 confirms the endpoint exists"
  }

  # Priorities are evaluated low to high. A blocked path that sorted after a
  # forwarding rule would never be reached.
  assert {
    condition = alltrue([
      for rule in values(aws_lb_listener_rule.web_blocked) : rule.priority <= 100
    ])
    error_message = "blocking rules must have lower priority numbers than any forwarding rule, or they are evaluated too late to block anything"
  }
}

# The WebSocket-critical setting. 120 against a 25s ping and a 10s pong wait.
run "idle_timeout_clears_the_realtime_ping_interval" {
  command = apply

  assert {
    condition     = aws_lb.this.idle_timeout >= 2 * (var.realtime_ping_interval_seconds + var.realtime_pong_timeout_seconds)
    error_message = "the idle timeout must clear the hub's ping interval plus its pong wait by a factor of two; below that the load balancer closes WebSockets the application believes are healthy, and nothing logs an error"
  }

  assert {
    condition     = aws_lb.this.idle_timeout > var.realtime_ping_interval_seconds
    error_message = "an idle timeout at or below the ping interval kills every live board on a timer"
  }
}

# AWS's own default is 60, which happens to work against a 25s ping and would
# stop working the moment somebody raised the ping interval to 40. The point of
# the validation is that raising one without the other is a plan error.
run "an_idle_timeout_below_the_ping_interval_is_rejected" {
  command = plan

  variables {
    idle_timeout_seconds = 20
  }

  expect_failures = [var.idle_timeout_seconds]
}

run "an_idle_timeout_that_only_just_clears_the_ping_is_still_rejected" {
  command = plan

  variables {
    # Above the 25s ping, below 2 x (25 + 10). Passing this would mean the
    # margin is decorative.
    idle_timeout_seconds = 45
  }

  expect_failures = [var.idle_timeout_seconds]
}

# The API is not on 443, and that is the whole of its exposure control: an ALB's
# security group is per-port, so sharing 443 with the web tier would mean
# sharing "open to the world" with it.
run "the_api_listener_is_not_the_public_listener" {
  command = apply

  assert {
    condition     = aws_lb_listener.api.port != aws_lb_listener.web.port
    error_message = "the API and the web tier must be on different listener ports; an ALB security group cannot distinguish two hostnames on one port, so sharing 443 would publish the API"
  }

  assert {
    condition     = aws_lb_listener.web.port == 443
    error_message = "the web tier is the product and belongs on 443"
  }

  assert {
    condition = alltrue([
      aws_lb_listener.web.protocol == "HTTPS",
      aws_lb_listener.api.protocol == "HTTPS",
    ])
    error_message = "both application listeners must terminate TLS; the web tier's session cookies are Secure unconditionally in a production build"
  }
}

run "plain_http_only_redirects" {
  command = apply

  assert {
    condition     = aws_lb_listener.http.default_action[0].type == "redirect"
    error_message = "the HTTP listener must forward nothing -- a page served over HTTP renders and then silently fails to hold a session"
  }

  assert {
    condition = alltrue([
      aws_lb_listener.http.default_action[0].redirect[0].protocol == "HTTPS",
      aws_lb_listener.http.default_action[0].redirect[0].status_code == "HTTP_301",
    ])
    error_message = "the HTTP listener must redirect permanently to HTTPS"
  }
}

# Stickiness is the reflex answer for WebSockets and the wrong one here: ADR
# 0005's Redis fan-out is what makes instances interchangeable, and a cookie
# would pin a client's ordinary HTTP requests to one task as well.
run "stickiness_is_off_on_both_target_groups" {
  command = apply

  assert {
    condition = alltrue([
      aws_lb_target_group.api.stickiness[0].enabled == false,
      aws_lb_target_group.web.stickiness[0].enabled == false,
    ])
    error_message = "stickiness must stay off: it is not what makes a WebSocket work, and on the web tier it would hide #69 rather than fix it"
  }

  # least_outstanding_requests, not round robin. A WebSocket is one request that
  # lasts for hours, so round robin would keep sending new connections to a task
  # that is already carrying the most of them.
  assert {
    condition     = aws_lb_target_group.api.load_balancing_algorithm_type == "least_outstanding_requests"
    error_message = "the API target group must balance on outstanding requests; with long-lived WebSockets, request-count round robin is blind to how many clients a task is already carrying"
  }
}

# The application's own drain budget is HTTP_SHUTDOWN_TIMEOUT, 15 seconds, and
# it covers the hub drain and the HTTP drain together. A shorter deregistration
# delay means the load balancer cuts connections the hub was about to close with
# a reconnect hint, which turns a rolling deploy into a reconnect storm.
run "deregistration_delay_exceeds_the_application_drain_budget" {
  command = apply

  assert {
    condition = alltrue([
      aws_lb_target_group.api.deregistration_delay > 15,
      aws_lb_target_group.web.deregistration_delay > 15,
    ])
    error_message = "deregistration_delay must exceed the API's 15s HTTP_SHUTDOWN_TIMEOUT, or a deploy resets sockets instead of draining them"
  }
}

run "rejects_one_availability_zone" {
  command = plan

  variables {
    public_subnet_ids = ["subnet-00000000000000001"]
  }

  expect_failures = [var.public_subnet_ids]
}

run "rejects_the_same_name_for_both_tiers" {
  command = plan

  variables {
    api_hostname = "staging.collabboard.test"
  }

  expect_failures = [var.api_hostname]
}

run "rejects_a_zone_name_where_a_zone_id_belongs" {
  command = plan

  variables {
    route53_zone_id = "collabboard.test"
  }

  expect_failures = [var.route53_zone_id]
}

run "rejects_an_empty_blocked_path_list" {
  command = plan

  variables {
    blocked_paths = []
  }

  expect_failures = [var.blocked_paths]
}
