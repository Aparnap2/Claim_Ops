# Original document bytes. Postgres stores the canonical object key
# (tenants/{tenant}/claims/{claim}/documents/{doc}/original), never gs:// URLs.

resource "google_storage_bucket" "documents" {
  name                        = "claimops-documents-${var.env}"
  location                    = var.bucket_location
  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  versioning {
    enabled = false # blobs are content-addressed and immutable; no versions needed
  }

  lifecycle_rule {
    condition {
      age = 90
    }
    action {
      type          = "SetStorageClass"
      storage_class = "NEARLINE"
    }
  }

  depends_on = [google_project_service.apis]
}
