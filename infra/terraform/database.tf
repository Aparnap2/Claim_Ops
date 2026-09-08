# Cloud SQL Postgres is the system of record (ADR: Postgres stays;
# no Firestore). Roles mirror the local grants: claimops_app is
# tenant-scoped via RLS, claimops_worker owns cross-tenant outbox dispatch.

resource "google_sql_database_instance" "main" {
  name                = "claimops-${var.env}"
  database_version    = "POSTGRES_16"
  region              = var.region
  deletion_protection = var.db_deletion_protection

  settings {
    tier = var.db_tier

    backup_configuration {
      enabled = true
    }

    ip_configuration {
      ipv4_enabled = true
      # Production hardening (private IP, authorized networks) is a
      # follow-up once the smoke project exists. Local parity does not
      # cover networking.
    }
  }

  depends_on = [google_project_service.apis]
}

resource "google_sql_database" "claimops" {
  name     = "claimops"
  instance = google_sql_database_instance.main.name
}

resource "google_sql_user" "app" {
  name     = "claimops_app"
  instance = google_sql_database_instance.main.name
  password = var.db_password_app
}

resource "google_sql_user" "worker" {
  name     = "claimops_worker"
  instance = google_sql_database_instance.main.name
  password = var.db_password_worker
}
