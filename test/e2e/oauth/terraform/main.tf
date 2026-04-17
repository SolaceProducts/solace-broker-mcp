terraform {
  required_version = ">= 1.0"

  required_providers {
    keycloak = {
      source  = "keycloak/keycloak"
      version = "~> 5.0"
    }
  }
}

# Keycloak Provider Configuration
provider "keycloak" {
  client_id     = "admin-cli"
  username      = var.keycloak_admin_username
  password      = var.keycloak_admin_password
  url           = "http://localhost:${var.keycloak_port}"
  initial_login = false
}

# Create Solace Realm
resource "keycloak_realm" "solace" {
  realm   = var.realm_name
  enabled = true

  # Access settings
  access_code_lifespan = "1m"

  # Token settings (1 day for development)
  access_token_lifespan           = "24h"
  sso_session_max_lifespan        = "24h"
  client_session_max_lifespan     = "24h"
}

# Create Client Scopes: solace:read
resource "keycloak_openid_client_scope" "solace_read" {
  realm_id               = keycloak_realm.solace.id
  name                   = "solace:read"
  description            = "Read access to Solace broker"
  include_in_token_scope = true
  gui_order              = 1
}

# Create Client Scopes: solace:write
resource "keycloak_openid_client_scope" "solace_write" {
  realm_id               = keycloak_realm.solace.id
  name                   = "solace:write"
  description            = "Write access to Solace broker"
  include_in_token_scope = true
  gui_order              = 2
}

# Create a dedicated client scope for audience claim
# This ensures ALL clients (including dynamically registered ones) get the audience claim
resource "keycloak_openid_client_scope" "mcp_audience" {
  realm_id               = keycloak_realm.solace.id
  name                   = "mcp-server-audience"
  description            = "Adds audience claim for MCP server validation"
  include_in_token_scope = true
  gui_order              = 3
}

# Add Audience Mapper to the client scope (not to a specific client)
# This applies to ALL clients that use this scope, including DCR clients
resource "keycloak_openid_audience_protocol_mapper" "mcp_audience_mapper" {
  realm_id                 = keycloak_realm.solace.id
  client_scope_id          = keycloak_openid_client_scope.mcp_audience.id
  name                     = "solace-mcp-audience-mapper"
  included_custom_audience = var.mcp_server_audience
  add_to_access_token      = true
}

# Assign custom scopes as default for all clients in the realm (including DCR)
resource "keycloak_realm_default_client_scopes" "solace_default_scopes" {
  realm_id = keycloak_realm.solace.id

  default_scopes = [
    keycloak_openid_client_scope.solace_read.name,
    keycloak_openid_client_scope.solace_write.name,
    keycloak_openid_client_scope.mcp_audience.name,
  ]
}

# Create MCP Client (with client credentials grant)
resource "keycloak_openid_client" "mcp_client" {
  realm_id  = keycloak_realm.solace.id
  client_id = var.mcp_client_id
  name      = "MCP Client"
  enabled   = true

  # Access type: confidential (enables client authentication)
  access_type = "CONFIDENTIAL"

  # Grant types
  standard_flow_enabled        = true   # Enable authorization code flow for browser-based auth
  implicit_flow_enabled        = false  # Disable implicit flow (deprecated in OAuth 2.1)
  direct_access_grants_enabled = false  # Disable direct access grants
  service_accounts_enabled     = true   # Keep client credentials for backward compatibility

  # Redirect URIs for browser-based OAuth flow
  # Claude MCP client uses these URIs to receive authorization codes
  valid_redirect_uris = [
    "http://localhost:*",  # Allow any localhost port for flexibility
    "http://127.0.0.1:*"   # IPv4 localhost
  ]

  # PKCE (Proof Key for Code Exchange) - OAuth 2.1 requirement
  pkce_code_challenge_method = "S256"  # Use SHA-256 for PKCE
}

# Assign custom scopes to the MCP client
resource "keycloak_openid_client_default_scopes" "mcp_client_scopes" {
  realm_id  = keycloak_realm.solace.id
  client_id = keycloak_openid_client.mcp_client.id

  default_scopes = [
    keycloak_openid_client_scope.solace_read.name,
    keycloak_openid_client_scope.solace_write.name,
    keycloak_openid_client_scope.mcp_audience.name,
  ]
}

# Create a test user for Phase 2 (Authorization Code + PKCE) testing
resource "keycloak_user" "test_user" {
  realm_id   = keycloak_realm.solace.id
  username   = var.test_user_username
  enabled    = true
  email      = var.test_user_email
  first_name = "Test"
  last_name  = "User"

  email_verified = true

  initial_password {
    value     = var.test_user_password
    temporary = false  # Don't require password change on first login
  }
}
