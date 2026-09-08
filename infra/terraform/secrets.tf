# Secret Manager holds DSNs; secret VALUES are added out-of-band
# (console, gcloud, or CI) — never in state, never in tfvars.

resource "google_secret_manager_secret" "database_url" {
  secret_id = "claimops-${var.env}-database-url"

  replication {
    auto {}
  }

  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret" "worker_database_url" {
  secret_id = "claimops-${var.env}-worker-database-url"

  replication {
    auto {}
  }
}
