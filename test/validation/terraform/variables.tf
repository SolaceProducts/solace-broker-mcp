variable "broker_url" {
  description = "SEMP base URL for the Solace broker"
  type        = string
}

variable "broker_username" {
  description = "Admin username for SEMP"
  type        = string
}

variable "broker_password" {
  description = "Admin password for SEMP"
  type        = string
  sensitive   = true
}
