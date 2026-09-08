# Pub/Sub topology: document.ingested topic, push subscription to the
# worker service (OIDC), DLQ topic for poison messages after
# push_max_delivery_attempts redeliveries.

resource "google_pubsub_topic" "documents" {
  name       = "document.ingested"
  depends_on = [google_project_service.apis]
}

resource "google_pubsub_topic" "documents_dlq" {
  name = "document.ingested-dlq"
}

resource "google_pubsub_subscription" "worker_push" {
  name  = "document.ingested-worker"
  topic = google_pubsub_topic.documents.id

  ack_deadline_seconds       = 60
  message_retention_duration = "604800s" # 7d

  push_config {
    push_endpoint = "${google_cloud_run_v2_service.worker.uri}/events/document-ingested"
    oidc_token {
      service_account_email = google_service_account.push_invoker.email
      # Static agreed audience (var.push_audience) instead of the worker URL:
      # avoids a create-time cycle (worker URI is computed) while keeping a
      # sender+receiver shared secret binding. Both sides must match.
      audience = var.push_audience
    }
  }

  retry_policy {
    minimum_backoff = "10s"
    maximum_backoff = "600s"
  }

  dead_letter_policy {
    dead_letter_topic     = google_pubsub_topic.documents_dlq.id
    max_delivery_attempts = var.push_max_delivery_attempts
  }

  expiration_policy {
    ttl = "" # never expire; an expired sub silently drops the pipeline
  }
}
