variable "kube_config_path" {
  description = "Path to kubernetes config file."
  type        = string
  default     = "~/.kube/config"
}

variable "aibrix_release_version" {
  description = "Release version of AIBrix to deploy."
  type        = string
  default     = "v0.2.0"
}