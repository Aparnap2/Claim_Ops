output "api_url" {
  description = "Cloud Run API service URL."
  value       = google_cloud_run_v2_service.api.uri
}

output "worker_url" {
  description = "Cloud Run worker service URL (push endpoint base)."
  value       = google_cloud_run_v2_service.worker.uri
}

output "topic" {
  description = "Document events topic."
  value       = google_pubsub_topic.documents.id
}

output "bucket" {
  description = "Document blobs bucket."
  value       = google_storage_bucket.documents.url
}
