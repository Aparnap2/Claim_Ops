# Three runtime identities (least privilege, mirrors the Postgres roles):
# api (serve + ingest + dispatch publish), worker (push consume + GCS read),
# push-invoker (Pub/Sub's OIDC identity when calling the worker).

resource "google_service_account" "api" {
  account_id   = "claimops-api-${var.env}"
  display_name = "ClaimOps API (${var.env})"
}

resource "google_service_account" "worker" {
  account_id   = "claimops-worker-${var.env}"
  display_name = "ClaimOps worker (${var.env})"
}

resource "google_service_account" "push_invoker" {
  account_id   = "claimops-push-${var.env}"
  display_name = "Pub/Sub push invoker (${var.env})"
}

# Worker push endpoint is authenticated: only the push invoker may call it.
resource "google_cloud_run_v2_service_iam_member" "worker_invoker" {
  name   = google_cloud_run_v2_service.worker.name
  role   = "roles/run.invoker"
  member = "serviceAccount:${google_service_account.push_invoker.email}"
}

# Dispatcher (api identity) publishes to the documents topic.
resource "google_pubsub_topic_iam_member" "api_publisher" {
  topic  = google_pubsub_topic.documents.name
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:${google_service_account.api.email}"
}

# API reads/writes blobs in the documents bucket only (objectAdmin scoped
# to this bucket: create/get/delete required by the ingest service).
resource "google_storage_bucket_iam_member" "api_blobs" {
  bucket = google_storage_bucket.documents.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.api.email}"
}

resource "google_storage_bucket_iam_member" "worker_blobs" {
  bucket = google_storage_bucket.documents.name
  role   = "roles/storage.objectViewer"
  member = "serviceAccount:${google_service_account.worker.email}"
}

# Secrets: api reads DATABASE_URL, worker reads WORKER_DATABASE_URL.
resource "google_secret_manager_secret_iam_member" "api_dsn" {
  secret_id = google_secret_manager_secret.database_url.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.api.email}"
}

resource "google_secret_manager_secret_iam_member" "worker_dsn" {
  secret_id = google_secret_manager_secret.worker_database_url.secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = "serviceAccount:${google_service_account.worker.email}"
}

# DLQ plumbing for the Google-managed Pub/Sub service agent: publish to the
# DLQ topic and acknowledge on the source subscription. Misconfiguring this
# fails silently in prod — the smoke test must land a poison event in DLQ.
data "google_project" "project" {}

resource "google_pubsub_topic_iam_member" "dlq_agent_publish" {
  topic  = google_pubsub_topic.documents_dlq.name
  role   = "roles/pubsub.publisher"
  member = "serviceAccount:service-${data.google_project.project.number}@gcp-sa-pubsub.iam.gserviceaccount.com"
}

resource "google_pubsub_subscription_iam_member" "dlq_agent_ack" {
  subscription = google_pubsub_subscription.worker_push.name
  role         = "roles/pubsub.subscriber"
  member       = "serviceAccount:service-${data.google_project.project.number}@gcp-sa-pubsub.iam.gserviceaccount.com"
}
