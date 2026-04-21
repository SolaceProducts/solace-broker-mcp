output "keycloak_url" {
  description = "Keycloak admin console URL"
  value       = "http://localhost:${var.keycloak_port}"
}

output "realm_name" {
  description = "Name of the created realm"
  value       = keycloak_realm.solace.realm
}

output "mcp_client_id" {
  description = "Client ID for the MCP public client (Phase 2: Authorization Code + PKCE)"
  value       = keycloak_openid_client.mcp_client_public.client_id
}

output "mcp_client_confidential_id" {
  description = "Client ID for the MCP confidential client (Phase 1: Client Credentials)"
  value       = keycloak_openid_client.mcp_client_confidential.client_id
}

output "mcp_client_secret" {
  description = "Client secret for the MCP confidential client (sensitive)"
  value       = keycloak_openid_client.mcp_client_confidential.client_secret
  sensitive   = true
}

output "token_endpoint" {
  description = "OAuth2 token endpoint"
  value       = "http://localhost:${var.keycloak_port}/realms/${var.realm_name}/protocol/openid-connect/token"
}

output "jwks_uri" {
  description = "JWKS endpoint for public keys"
  value       = "http://localhost:${var.keycloak_port}/realms/${var.realm_name}/protocol/openid-connect/certs"
}

output "openid_configuration" {
  description = "OpenID Connect discovery endpoint"
  value       = "http://localhost:${var.keycloak_port}/realms/${var.realm_name}/.well-known/openid-configuration"
}

output "issuer" {
  description = "Token issuer (for AUTH_ISSUER in MCP server)"
  value       = "http://localhost:${var.keycloak_port}/realms/${var.realm_name}"
}

output "audience" {
  description = "Token audience (for AUTH_AUDIENCE in MCP server)"
  value       = var.mcp_server_audience
}

output "test_token_command" {
  description = "Curl command to get a test access token (Phase 1: Client Credentials)"
  value       = <<-EOT
    curl -X POST http://localhost:${var.keycloak_port}/realms/${var.realm_name}/protocol/openid-connect/token \
      -H "Content-Type: application/x-www-form-urlencoded" \
      -d "grant_type=client_credentials" \
      -d "client_id=${var.mcp_client_id}-confidential" \
      -d "client_secret=<USE_OUTPUT_mcp_client_secret>" \
      -d "scope=solace:read solace:write"
  EOT
}

output "authorization_endpoint" {
  description = "OAuth2 authorization endpoint (for Phase 2: Authorization Code + PKCE)"
  value       = "http://localhost:${var.keycloak_port}/realms/${var.realm_name}/protocol/openid-connect/auth"
}

output "test_user_username" {
  description = "Test user username (for Phase 2 interactive login)"
  value       = var.test_user_username
}

output "test_user_password" {
  description = "Test user password (for Phase 2 interactive login)"
  value       = var.test_user_password
  sensitive   = true
}

output "phase2_login_url" {
  description = "URL to initiate Phase 2 OAuth flow (Authorization Code + PKCE)"
  value       = <<-EOT
    http://localhost:${var.keycloak_port}/realms/${var.realm_name}/protocol/openid-connect/auth?client_id=${var.mcp_client_id}&response_type=code&scope=solace:read%20solace:write&redirect_uri=http://localhost:8080/callback&code_challenge=REPLACE_WITH_PKCE_CHALLENGE&code_challenge_method=S256
  EOT
}

output "keycloak_port" {
  description = "Keycloak HTTP port"
  value       = var.keycloak_port
}

output "keycloak_admin_username" {
  description = "Keycloak admin username"
  value       = var.keycloak_admin_username
}

output "keycloak_admin_password" {
  description = "Keycloak admin password"
  value       = var.keycloak_admin_password
  sensitive   = true
}
