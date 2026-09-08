# Dev smoke project. plan/apply needs real credentials + billing.
project_id = "claimops-dev-TODO"
env        = "dev"

api_image    = "asia-south1-docker.pkg.dev/claimops-dev-TODO/claimops-dev/api:dev"
worker_image = "asia-south1-docker.pkg.dev/claimops-dev-TODO/claimops-dev/worker:dev"

db_tier                = "db-f1-micro"
db_deletion_protection = false
worker_min_instances   = 0
