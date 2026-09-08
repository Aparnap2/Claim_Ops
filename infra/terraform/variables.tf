variable "project_id" {
  description = "GCP project id (dev or prod)."
  type        = string
}

variable "region" {
  description = "GCP region for regional resources."
  type        = string
  default     = "asia-south1"
}

variable "env" {
  description = "Environment name (dev or prod). Used in resource names."
  type        = string
}

variable "api_image" {
  description = "Artifact Registry image URL for the API service."
  type        = string
}

variable "worker_image" {
  description = "Artifact Registry image URL for the worker service."
  type        = string
}

variable "bucket_location" {
  description = "GCS bucket location."
  type        = string
  default     = "ASIA-SOUTH1"
}

variable "db_tier" {
  description = "Cloud SQL machine tier."
  type        = string
  default     = "db-f1-micro"
}

variable "db_deletion_protection" {
  description = "Cloud SQL deletion protection (true in prod)."
  type        = bool
  default     = false
}

variable "db_password_app" {
  description = "Password for the claimops_app role. Set via TF_VAR (never committed)."
  type        = string
  sensitive   = true
  default     = ""
}

variable "db_password_worker" {
  description = "Password for the claimops_worker role. Set via TF_VAR (never committed)."
  type        = string
  sensitive   = true
  default     = ""
}

variable "worker_min_instances" {
  description = "Worker min instances (0 in dev, >=1 only if pull is ever used in prod)."
  type        = number
  default     = 0
}

variable "push_max_delivery_attempts" {
  description = "Pub/Sub redeliveries before DLQ."
  type        = number
  default     = 5
}

variable "push_audience" {
  description = "Agreed OIDC audience for push delivery (static to avoid a create-time cycle with the worker URL)."
  type        = string
  default     = "claimops-worker"
}
