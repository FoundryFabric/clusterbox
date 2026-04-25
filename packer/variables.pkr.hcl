variable "hetzner_api_token" {
  description = "Hetzner Cloud API token with read/write permissions."
  type        = string
  sensitive   = true
}

variable "version" {
  description = "Snapshot version string, used in the snapshot name (e.g. '0.1.0')."
  type        = string
  default     = "0.1.0"
}

variable "ssh_public_key" {
  description = "Public SSH key to inject for the clusterbox user."
  type        = string
}
