# Cloud Run services. Squeezed by default to match the local caps:
# 512Mi / 1 CPU each; GOMEMLIMIT keeps the Go runtime inside the cgroup.

resource "google_cloud_run_v2_service" "api" {
  name     = "claimops-api-${var.env}"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL"

  template {
    service_account = google_service_account.api.email

    containers {
      image = var.api_image

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }

      ports {
        container_port = 8080
      }

      env {
        name  = "APP_ENV"
        value = var.env == "prod" ? "prod" : "local"
      }
      env {
        name  = "GCP_PROJECT"
        value = var.project_id
      }
      env {
        name  = "GCS_BUCKET_DOCUMENTS"
        value = google_storage_bucket.documents.name
      }
      env {
        name  = "PUBSUB_TOPIC_DOCUMENTS"
        value = google_pubsub_topic.documents.name
      }
      env {
        name = "DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.database_url.secret_id
            version = "latest"
          }
        }
      }
      env {
        name = "WORKER_DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.worker_database_url.secret_id
            version = "latest"
          }
        }
      }
    }

    scaling {
      min_instance_count = 0
      max_instance_count = 4
    }
  }

  depends_on = [google_project_service.apis]
}

resource "google_cloud_run_v2_service" "worker" {
  name     = "claimops-worker-${var.env}"
  location = var.region
  ingress  = "INGRESS_TRAFFIC_ALL" # authenticated via OIDC (push invoker only)

  template {
    service_account = google_service_account.worker.email

    containers {
      image = var.worker_image

      resources {
        limits = {
          cpu    = "1"
          memory = "512Mi"
        }
      }

      ports {
        container_port = 8080
      }

      env {
        name  = "APP_ENV"
        value = var.env == "prod" ? "prod" : "local"
      }
      env {
        name  = "GCP_PROJECT"
        value = var.project_id
      }
      env {
        name  = "GCS_BUCKET_DOCUMENTS"
        value = google_storage_bucket.documents.name
      }
      env {
        name  = "PUSH_AUTH_MODE"
        value = "oidc"
      }
      env {
        name  = "PUSH_AUDIENCE"
        value = var.push_audience
      }
      env {
        name  = "PUSH_SERVICE_ACCOUNT"
        value = google_service_account.push_invoker.email
      }
      env {
        name = "DATABASE_URL"
        value_source {
          secret_key_ref {
            secret  = google_secret_manager_secret.worker_database_url.secret_id
            version = "latest"
          }
        }
      }
    }

    scaling {
      min_instance_count = var.worker_min_instances
      max_instance_count = 4
    }
  }
}
