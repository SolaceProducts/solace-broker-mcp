variable "keycloak_port" {
  description = "Port to expose Keycloak on localhost"
  type        = number
  default     = 8090
}

variable "keycloak_admin_username" {
  description = "Keycloak admin username"
  type        = string
  default     = "admin"
}

variable "keycloak_admin_password" {
  description = "Keycloak admin password"
  type        = string
  default     = "admin"
  sensitive   = true
}

variable "realm_name" {
  description = "Name of the Keycloak realm to create"
  type        = string
  default     = "solace"
}

variable "mcp_client_id" {
  description = "Client ID for the MCP client"
  type        = string
  default     = "mcp-client"
}

variable "mcp_server_audience" {
  description = "Audience value to include in access tokens for MCP server validation"
  type        = string
  default     = "solace-mcp-server"
}

variable "test_user_username" {
  description = "Username for the test user (Phase 2 testing)"
  type        = string
  default     = "testuser"
}

variable "test_user_email" {
  description = "Email for the test user"
  type        = string
  default     = "testuser@example.com"
}

variable "test_user_password" {
  description = "Password for the test user"
  type        = string
  default     = "testpass123"
  sensitive   = true
}
