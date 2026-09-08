# ClaimOps AI — Terraform root (used resources only, ADR-006).
# Local parity: same modules via environments/*.tfvars against localgcp.
# No Firestore / Workflows / Redis / GKE / Vertex in this phase.

terraform {
  required_version = ">= 1.6.0"
  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 6.0"
    }
  }
  # Backend is configured per environment (GCS). Local validate uses
  # init -backend=false; no state is written by fmt/validate.
}

provider "google" {
  project = var.project_id
  region  = var.region
}
