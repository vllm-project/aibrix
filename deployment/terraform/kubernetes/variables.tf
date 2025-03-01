variable "aibrix_release_version" {
  description = "Release version of AIBrix to deploy."
  type        = string
  default     = "v0.2.0"
}

variable "deploy_model" {
  description = "Whether to deploy example model."
  type = bool
  default = true
}