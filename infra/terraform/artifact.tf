# Container images live here; Cloud Run pulls from these URLs.

resource "google_artifact_registry_repository" "images" {
  location      = var.region
  repository_id = "claimops-${var.env}"
  format        = "DOCKER"
  depends_on    = [google_project_service.apis]
}
