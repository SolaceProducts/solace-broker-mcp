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

# =============================================================================
# Phase 1: Confidential Client (Client Credentials Flow)
# =============================================================================
# This client is used for service-to-service authentication
# Uses client_id + client_secret to obtain access tokens
resource "keycloak_openid_client" "mcp_client_confidential" {
  realm_id  = keycloak_realm.solace.id
  client_id = "${var.mcp_client_id}-confidential"
  name      = "MCP Client (Confidential)"
  enabled   = true

  # Access type: confidential (requires client secret)
  access_type = "CONFIDENTIAL"

  # Grant types
  standard_flow_enabled        = false  # Disable authorization code flow
  implicit_flow_enabled        = false  # Disable implicit flow
  direct_access_grants_enabled = false  # Disable direct access grants
  service_accounts_enabled     = true   # Enable client credentials flow
}

# =============================================================================
# Phase 2: Public Client (Authorization Code + PKCE Flow)
# =============================================================================
# This client is used for browser-based authentication (MCP clients like Claude Desktop)
# Uses Authorization Code + PKCE without client secret
resource "keycloak_openid_client" "mcp_client_public" {
  realm_id  = keycloak_realm.solace.id
  client_id = var.mcp_client_id
  name      = "MCP Client (Public)"
  enabled   = true

  # Access type: public (for browser-based OAuth with PKCE, no client secret)
  access_type = "PUBLIC"

  # Grant types
  standard_flow_enabled        = true   # Enable authorization code flow for browser-based auth
  implicit_flow_enabled        = false  # Disable implicit flow (deprecated in OAuth 2.1)
  direct_access_grants_enabled = false  # Disable direct access grants
  service_accounts_enabled     = false  # Public clients cannot use service accounts

  # Redirect URIs for browser-based OAuth flow
  # Claude MCP client uses these URIs to receive authorization codes
  valid_redirect_uris = [
    "http://localhost:*",  # Allow any localhost port for flexibility
    "http://127.0.0.1:*"   # IPv4 localhost
  ]

  # PKCE (Proof Key for Code Exchange) - OAuth 2.1 requirement
  pkce_code_challenge_method = "S256"  # Use SHA-256 for PKCE
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
