# The environment's only entry point. One load balancer, three listener ports,
# and the port is what decides who can reach what:
#
#   80    -> redirect to 443. Open to the internet.
#   443   -> the Next.js web tier. Open to the internet. This is the product.
#   8443  -> the Go API. Open only to `api_ingress_cidrs`, which in practice is
#            this environment's own NAT gateway address, because the API's only
#            client is the web tier (ADR 0010, ADR 0014).
#
# Two properties in here are easy to break and expensive to notice:
#
#   1. `idle_timeout` versus the realtime hub's ping interval. See
#      variables.tf; the relationship is validated rather than commented.
#   2. `/healthz` and `/metrics` are answered with a fixed 404 by both HTTPS
#      listeners, while the target groups still health-check `/healthz`. A
#      target group health check is issued directly to the target's address and
#      does not traverse a listener, so blocking the path publicly does not
#      blind the health check. Both facts are asserted in tests/.

# ---------------------------------------------------------------------------
# Certificate
# ---------------------------------------------------------------------------

# One certificate covering both names. `create_before_destroy` because a
# certificate in use by a listener cannot be deleted, so any change that replaces
# it -- adding a third hostname, most likely -- must issue the new one first.
resource "aws_acm_certificate" "this" {
  domain_name               = var.web_hostname
  subject_alternative_names = [var.api_hostname]
  validation_method         = "DNS"

  tags = { Name = var.name_prefix }

  lifecycle {
    create_before_destroy = true
  }
}

# Keyed on the hostnames, which are variables and therefore known at plan time,
# rather than on `domain_validation_options` directly.
#
# The documented pattern iterates the validation options themselves. That relies
# on the AWS provider populating a Computed attribute during plan -- which it
# does, but it means the shape of the plan depends on provider internals rather
# than on anything in this file, and it is unplannable the moment that stops
# being true (it is already unplannable under a mocked provider, which is how
# this was found). Static keys with apply-time values is what the error message
# for that failure recommends, and it costs one lookup.
resource "aws_route53_record" "certificate_validation" {
  for_each = toset(distinct([var.web_hostname, var.api_hostname]))

  zone_id = var.route53_zone_id

  name = one([
    for option in aws_acm_certificate.this.domain_validation_options :
    option.resource_record_name if option.domain_name == each.key
  ])

  type = one([
    for option in aws_acm_certificate.this.domain_validation_options :
    option.resource_record_type if option.domain_name == each.key
  ])

  records = [
    one([
      for option in aws_acm_certificate.this.domain_validation_options :
      option.resource_record_value if option.domain_name == each.key
    ])
  ]

  ttl = 60

  # A validation record left over from a previous certificate has the same name
  # and a different value, and Route 53 would otherwise refuse to overwrite it.
  allow_overwrite = true
}

# Not a real AWS resource -- it blocks until ACM reports the certificate issued,
# so the listeners below cannot be created against a certificate that is still
# PENDING_VALIDATION. Without it, apply fails on the listener with an error about
# the certificate rather than about DNS, which points at the wrong thing.
resource "aws_acm_certificate_validation" "this" {
  certificate_arn         = aws_acm_certificate.this.arn
  validation_record_fqdns = [for record in aws_route53_record.certificate_validation : record.fqdn]
}

# ---------------------------------------------------------------------------
# Load balancer
# ---------------------------------------------------------------------------

resource "aws_lb" "this" {
  name               = var.name_prefix
  load_balancer_type = "application"
  internal           = false
  subnets            = var.public_subnet_ids
  security_groups    = [var.security_group_id]

  idle_timeout = var.idle_timeout_seconds

  # Refuses a request carrying a header name or value that is not valid HTTP,
  # which is the cheap half of request-smuggling defence. It does not affect the
  # WebSocket handshake: `Sec-WebSocket-Protocol`, which is how the API's bearer
  # token arrives (apps/api/internal/api/realtime.go), is a well-formed header.
  drop_invalid_header_fields = true

  enable_deletion_protection = var.enable_deletion_protection

  # HTTP/2 for browser traffic. It does not interfere with the WebSocket path:
  # an upgrade is an HTTP/1.1 mechanism, so a client that wants one negotiates
  # 1.1, and the Node client in the web tier speaks 1.1 unconditionally.
  enable_http2 = true

  tags = { Name = var.name_prefix }
}

resource "aws_route53_record" "web" {
  zone_id = var.route53_zone_id
  name    = var.web_hostname
  type    = "A"

  alias {
    name                   = aws_lb.this.dns_name
    zone_id                = aws_lb.this.zone_id
    evaluate_target_health = false
  }
}

resource "aws_route53_record" "api" {
  zone_id = var.route53_zone_id
  name    = var.api_hostname
  type    = "A"

  alias {
    name                   = aws_lb.this.dns_name
    zone_id                = aws_lb.this.zone_id
    evaluate_target_health = false
  }
}

# ---------------------------------------------------------------------------
# Target groups
# ---------------------------------------------------------------------------

# `name` rather than the `name_prefix` + create_before_destroy pattern used
# elsewhere in this repository: aws_lb_target_group's name_prefix is capped at
# six characters, which cannot hold `collabboard-staging`. The cost is that a
# change replacing a target group -- a port or protocol change, essentially --
# has to be done in two applies, because a group attached to a listener cannot be
# deleted. That is rare and loud; a six-character name would be permanent and
# unreadable.
resource "aws_lb_target_group" "api" {
  name        = "${var.name_prefix}-api"
  vpc_id      = var.vpc_id
  port        = var.api_container_port
  protocol    = "HTTP"
  target_type = "ip"

  deregistration_delay = var.deregistration_delay_seconds

  # Long-lived WebSockets are why this is not round robin. Round robin counts
  # requests, and a WebSocket is one request that lasts for hours -- so a task
  # that came up after a scale-out would receive an equal share of *new*
  # connections while carrying none of the old ones for as long as the imbalance
  # lasted. `least_outstanding_requests` counts connections still in flight,
  # which for this service is the same thing as connected clients.
  load_balancing_algorithm_type = "least_outstanding_requests"

  # Off, deliberately, and the reason is ADR 0005. Stickiness is the reflex
  # answer for WebSockets and it is the wrong one here: a WebSocket is a single
  # TCP connection pinned to a target for its lifetime, so nothing about it needs
  # a cookie, and the fan-out that makes any instance able to serve any board is
  # Redis pub/sub. Turning stickiness on would pin a client to an instance for
  # its *HTTP* requests too, which is how you end up with a hot task and a load
  # balancer that will not rebalance it.
  stickiness {
    enabled = false
    type    = "lb_cookie"
  }

  health_check {
    enabled = true
    path    = var.health_check_path
    port    = "traffic-port"
    matcher = "200"

    # The API's /healthz probes Postgres and Redis with a 2s budget each and
    # answers 503 if either is unreachable, so this is a readiness check, not a
    # liveness one -- a task with a broken database is taken out of rotation
    # rather than restarted, which is correct: restarting it would not fix the
    # database.
    interval            = 30
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = { Name = "${var.name_prefix}-api" }
}

resource "aws_lb_target_group" "web" {
  name        = "${var.name_prefix}-web"
  vpc_id      = var.vpc_id
  port        = var.web_container_port
  protocol    = "HTTP"
  target_type = "ip"

  deregistration_delay = var.deregistration_delay_seconds

  # Off here too, and this one is worth stating because it is the case where
  # stickiness would paper over a real bug rather than fix it. #69 is a refresh
  # token rotation race between web processes; binding a browser to one task
  # would hide it until a task was replaced. The web service runs a single task
  # for now instead, which is a stated constraint rather than a disguised one.
  stickiness {
    enabled = false
    type    = "lb_cookie"
  }

  health_check {
    enabled = true
    path    = var.health_check_path
    port    = "traffic-port"
    matcher = "200"

    # The web tier's /healthz deliberately does not probe the API, so an API
    # outage does not deregister every web task as well and turn one tier's
    # failure into two. See apps/web/lib/readiness.ts.
    interval            = 30
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = { Name = "${var.name_prefix}-web" }
}

# ---------------------------------------------------------------------------
# Listeners
# ---------------------------------------------------------------------------

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = var.http_port
  protocol          = "HTTP"

  # Everything, permanently. No path is served over plain HTTP -- the web tier's
  # session cookies are `Secure` unconditionally in a production build, so a page
  # served over HTTP would render and then silently fail to hold a session.
  default_action {
    type = "redirect"

    redirect {
      protocol    = "HTTPS"
      port        = tostring(var.web_port)
      status_code = "HTTP_301"
    }
  }
}

resource "aws_lb_listener" "web" {
  load_balancer_arn = aws_lb.this.arn
  port              = var.web_port
  protocol          = "HTTPS"
  ssl_policy        = var.ssl_policy
  certificate_arn   = aws_acm_certificate_validation.this.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.web.arn
  }
}

resource "aws_lb_listener" "api" {
  load_balancer_arn = aws_lb.this.arn
  port              = var.api_port
  protocol          = "HTTPS"
  ssl_policy        = var.ssl_policy
  certificate_arn   = aws_acm_certificate_validation.this.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }
}

# ---------------------------------------------------------------------------
# The rules that keep /healthz and /metrics off the internet
# ---------------------------------------------------------------------------
#
# Lowest priorities, so they are evaluated before anything that forwards. Present
# on BOTH HTTPS listeners rather than only the public one: the API listener is
# already restricted by address, and a control that only works when the other
# control is also working is one control.
#
# `/metrics` does not exist in the API yet (#12). Blocking it now is the point --
# when #12 adds it, it arrives unreachable rather than arriving published, and
# whoever wants it published has to delete a line that says why it is there.
#
# `/healthz` does exist and answers with the raw error from each failed
# dependency probe (#31). It is also the target groups' health check path, which
# is not a contradiction: see variables.tf, `health_check_path`.

resource "aws_lb_listener_rule" "web_blocked" {
  for_each = { for index, path in var.blocked_paths : path => index }

  listener_arn = aws_lb_listener.web.arn
  priority     = (each.value + 1) * 10

  action {
    type = "fixed-response"

    fixed_response {
      content_type = "text/plain"
      message_body = "Not Found"
      status_code  = "404"
    }
  }

  condition {
    path_pattern {
      values = [each.key, "${each.key}/*"]
    }
  }
}

resource "aws_lb_listener_rule" "api_blocked" {
  for_each = { for index, path in var.blocked_paths : path => index }

  listener_arn = aws_lb_listener.api.arn
  priority     = (each.value + 1) * 10

  action {
    type = "fixed-response"

    fixed_response {
      content_type = "text/plain"
      message_body = "Not Found"
      status_code  = "404"
    }
  }

  condition {
    path_pattern {
      values = [each.key, "${each.key}/*"]
    }
  }
}

# ---------------------------------------------------------------------------
# Alerting
#
# The Observability standard asks for at least one meaningful alert per project,
# wired to something concrete. Three, at $0.10/month each: the error rate the
# users see, and whether either tier has any capacity left.
#
# The topic has no subscription. Terraform can create one but cannot confirm it
# -- an email subscription is confirmed by clicking a link -- so a subscription
# resource here would sit permanently "pending confirmation" and look configured
# while notifying nobody. The ARN is an output and OPERATOR-INPUTS.md carries the
# one command that subscribes to it.
# ---------------------------------------------------------------------------

resource "aws_sns_topic" "alarms" {
  name = "${var.name_prefix}-alarms"

  tags = { Name = "${var.name_prefix}-alarms" }
}

resource "aws_cloudwatch_metric_alarm" "target_5xx" {
  alarm_name        = "${var.name_prefix}-target-5xx"
  alarm_description = "Targets behind ${var.name_prefix} are returning 5xx responses. This is the error-rate signal for both tiers; the load balancer's own 5xx (HTTPCode_ELB_5XX_Count) would mean the ALB could not reach a target at all."

  namespace   = "AWS/ApplicationELB"
  metric_name = "HTTPCode_Target_5XX_Count"
  statistic   = "Sum"
  period      = 300
  dimensions  = { LoadBalancer = aws_lb.this.arn_suffix }

  comparison_operator = "GreaterThanThreshold"
  threshold           = var.target_5xx_alarm_threshold
  evaluation_periods  = 1

  # No 5xx at all publishes no datapoint rather than a zero, so the default
  # (`missing`) would leave the alarm in INSUFFICIENT_DATA whenever the service
  # is healthy -- which reads like a broken alarm.
  treat_missing_data = "notBreaching"

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]

  tags = { Name = "${var.name_prefix}-target-5xx" }
}

resource "aws_cloudwatch_metric_alarm" "api_unhealthy_hosts" {
  alarm_name        = "${var.name_prefix}-api-unhealthy-hosts"
  alarm_description = "At least one API task is failing its health check. With a minimum of two tasks this is a warning; at two it is an outage."

  namespace   = "AWS/ApplicationELB"
  metric_name = "UnHealthyHostCount"
  statistic   = "Maximum"
  period      = 60
  dimensions = {
    LoadBalancer = aws_lb.this.arn_suffix
    TargetGroup  = aws_lb_target_group.api.arn_suffix
  }

  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = 1
  evaluation_periods  = 3

  # `missing` here, not notBreaching: no datapoint on this metric means the load
  # balancer is reporting nothing about the target group, which is not the same
  # as reporting zero unhealthy hosts and should not read as healthy.
  treat_missing_data = "missing"

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]

  tags = { Name = "${var.name_prefix}-api-unhealthy-hosts" }
}

resource "aws_cloudwatch_metric_alarm" "web_unhealthy_hosts" {
  alarm_name        = "${var.name_prefix}-web-unhealthy-hosts"
  alarm_description = "The web task is failing its health check. The web tier runs a single task until #69 is fixed, so one unhealthy host here is the whole product being down."

  namespace   = "AWS/ApplicationELB"
  metric_name = "UnHealthyHostCount"
  statistic   = "Maximum"
  period      = 60
  dimensions = {
    LoadBalancer = aws_lb.this.arn_suffix
    TargetGroup  = aws_lb_target_group.web.arn_suffix
  }

  comparison_operator = "GreaterThanOrEqualToThreshold"
  threshold           = 1
  evaluation_periods  = 3
  treat_missing_data  = "missing"

  alarm_actions = [aws_sns_topic.alarms.arn]
  ok_actions    = [aws_sns_topic.alarms.arn]

  tags = { Name = "${var.name_prefix}-web-unhealthy-hosts" }
}
