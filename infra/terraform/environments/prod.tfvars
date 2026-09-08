# Prod values. Set TF_VAR_db_password_app/worker from the vault at apply
# time; never commit secrets. Deletion protection stays on.
project_id = "claimops-prod-TODO"
env        = "prod"

api_image    = "asia-south1-docker.pkg.dev/claimops-prod-TODO/claimops-prod/api:RELEASE"
worker_image = "asia-south1-docker.pkg.dev/claimops-prod-TODO/claimops-prod/worker:RELEASE"

db_tier                = "db-custom-2-7680"
db_deletion_protection = true
worker_min_instances   = 0
