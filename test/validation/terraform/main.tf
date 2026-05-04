terraform {
  required_providers {
    solacebroker = {
      source = "registry.terraform.io/solaceproducts/solacebroker"
    }
  }
}

provider "solacebroker" {
  username = var.broker_username
  password = var.broker_password
  url      = var.broker_url
}

# ── VPNs (3 total — Docker VMR limit) ───────────────────────────────────────

resource "solacebroker_msg_vpn" "default" {
  msg_vpn_name                                = "default"
  enabled                                     = true
  max_msg_spool_usage                         = 1500
  authentication_basic_type                   = "internal"
  service_rest_incoming_plain_text_enabled     = true
  service_rest_incoming_plain_text_listen_port = 9000
}

resource "solacebroker_msg_vpn" "active" {
  msg_vpn_name        = "val-vpn-active"
  enabled             = true
  max_msg_spool_usage = 100
}

resource "solacebroker_msg_vpn" "disabled" {
  msg_vpn_name        = "val-vpn-disabled"
  enabled             = false
  max_msg_spool_usage = 0
}

# ── Client Username (default VPN) ───────────────────────────────────────────

resource "solacebroker_msg_vpn_client_username" "val_user" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  client_username = "val-user"
  password        = "valpass"
  enabled         = true
}

# ── Queues (default VPN) ────────────────────────────────────────────────────

resource "solacebroker_msg_vpn_queue" "healthy" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-healthy"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue" "backlog" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-backlog"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue_subscription" "backlog_sub" {
  msg_vpn_name       = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name         = solacebroker_msg_vpn_queue.backlog.queue_name
  subscription_topic = "backlog/>"
}

resource "solacebroker_msg_vpn_queue" "large_backlog" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-large-backlog"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue_subscription" "large_backlog_sub" {
  msg_vpn_name       = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name         = solacebroker_msg_vpn_queue.large_backlog.queue_name
  subscription_topic = "load/>"
}

resource "solacebroker_msg_vpn_queue" "egress_down" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-egress-down"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = false
}

resource "solacebroker_msg_vpn_queue_subscription" "egress_down_sub" {
  msg_vpn_name       = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name         = solacebroker_msg_vpn_queue.egress_down.queue_name
  subscription_topic = "stuck/>"
}

resource "solacebroker_msg_vpn_queue" "exclusive" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-exclusive"
  access_type     = "exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue" "with_consumer" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-with-consumer"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue" "rdp_events" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-rdp-events"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue" "rdp_alerts" {
  msg_vpn_name    = solacebroker_msg_vpn.default.msg_vpn_name
  queue_name      = "val-q-rdp-alerts"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

# ── Queues (val-vpn-active) — gives list-queues something to show there ─────

resource "solacebroker_msg_vpn_queue" "active_orders" {
  msg_vpn_name    = solacebroker_msg_vpn.active.msg_vpn_name
  queue_name      = "val-q-orders"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue" "active_invoices" {
  msg_vpn_name    = solacebroker_msg_vpn.active.msg_vpn_name
  queue_name      = "val-q-invoices"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

resource "solacebroker_msg_vpn_queue" "active_notifications" {
  msg_vpn_name    = solacebroker_msg_vpn.active.msg_vpn_name
  queue_name      = "val-q-notifications"
  access_type     = "non-exclusive"
  permission      = "consume"
  ingress_enabled = true
  egress_enabled  = true
}

# ── REST Delivery Point (default VPN) ───────────────────────────────────────

resource "solacebroker_msg_vpn_rest_delivery_point" "val_rdp" {
  msg_vpn_name             = solacebroker_msg_vpn.default.msg_vpn_name
  rest_delivery_point_name = "val-rdp"
  enabled                  = true
}

resource "solacebroker_msg_vpn_rest_delivery_point_queue_binding" "events" {
  msg_vpn_name             = solacebroker_msg_vpn.default.msg_vpn_name
  rest_delivery_point_name = solacebroker_msg_vpn_rest_delivery_point.val_rdp.rest_delivery_point_name
  queue_binding_name       = solacebroker_msg_vpn_queue.rdp_events.queue_name
  post_request_target      = "/api/events"
}

resource "solacebroker_msg_vpn_rest_delivery_point_queue_binding" "alerts" {
  msg_vpn_name             = solacebroker_msg_vpn.default.msg_vpn_name
  rest_delivery_point_name = solacebroker_msg_vpn_rest_delivery_point.val_rdp.rest_delivery_point_name
  queue_binding_name       = solacebroker_msg_vpn_queue.rdp_alerts.queue_name
  post_request_target      = "/api/alerts"
}

resource "solacebroker_msg_vpn_rest_delivery_point_rest_consumer" "primary" {
  msg_vpn_name             = solacebroker_msg_vpn.default.msg_vpn_name
  rest_delivery_point_name = solacebroker_msg_vpn_rest_delivery_point.val_rdp.rest_delivery_point_name
  rest_consumer_name       = "val-consumer-primary"
  remote_host              = "localhost"
  remote_port              = 18080
  tls_enabled              = false
  enabled                  = true
}

resource "solacebroker_msg_vpn_rest_delivery_point_rest_consumer" "secondary" {
  msg_vpn_name             = solacebroker_msg_vpn.default.msg_vpn_name
  rest_delivery_point_name = solacebroker_msg_vpn_rest_delivery_point.val_rdp.rest_delivery_point_name
  rest_consumer_name       = "val-consumer-secondary"
  remote_host              = "localhost"
  remote_port              = 18081
  tls_enabled              = false
  enabled                  = false
}
