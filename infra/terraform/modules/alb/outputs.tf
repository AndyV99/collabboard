output "dns_name" {
  description = "The load balancer's own DNS name. Both hostnames are aliases of it; useful when a certificate or a Route 53 record is being debugged and the alias is the suspect."
  value       = aws_lb.this.dns_name
}

output "web_url" {
  description = "Public entry point for the product."
  value       = "https://${var.web_hostname}"
}

output "api_url" {
  description = "Base URL of the Go API, including the non-default port. This is the value the web task definition's API_URL is set from, and it resolves publicly but only answers callers in api_ingress_cidrs."
  value       = "https://${var.api_hostname}:${var.api_port}"
}

output "api_target_group_arn" {
  description = "Target group the API service registers into."
  value       = aws_lb_target_group.api.arn
}

output "web_target_group_arn" {
  description = "Target group the web service registers into."
  value       = aws_lb_target_group.web.arn
}

output "idle_timeout_seconds" {
  description = "Seconds of silence before the load balancer closes a connection. Exported because it is the setting most likely to be blamed, correctly, when realtime works locally and not when deployed."
  value       = aws_lb.this.idle_timeout
}

output "certificate_arn" {
  description = "ARN of the issued ACM certificate covering both hostnames."
  value       = aws_acm_certificate_validation.this.certificate_arn
}

output "alarm_topic_arn" {
  description = "SNS topic the ALB alarms publish to. It has no subscription -- see main.tf and OPERATOR-INPUTS.md step 14."
  value       = aws_sns_topic.alarms.arn
}

output "listener_arns" {
  description = "Listener ARNs by role, so #103 or a future rule can attach to one by name rather than by port number."
  value = {
    http = aws_lb_listener.http.arn
    web  = aws_lb_listener.web.arn
    api  = aws_lb_listener.api.arn
  }
}
