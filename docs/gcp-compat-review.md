# GCP compatibility review — ClaimOps worker/event path

Date: 2026-09-06 · Status: review (not an ADR — no decision taken yet)
Trigger: localgcp 0.6.0 running locally (Pub/Sub gRPC :8085, Storage REST :4443, …).
Question asked: is the current implementation GCP-compatible, and has it been tested against localgcp?

## Verdict

**No to both.** Nothing in the repo has ever spoken to GCP or to the emulator:

- `apps/api/go.mod` contains zero `cloud.google.com/*` dependencies (verified).
- The event path is `InMemoryBus` only — publish, synchronous in-process dispatch,
  zero network. There is no transport adapter of any kind.
- `infra/` contains only `postgres/`. No Terraform, no emulator config, no
  service/container definitions, no `localgcp env` integration in the Makefile.
- Endpoints are hardcoded localhost fallbacks (`POLICY_BASE_URL → :3001`,
  postgres `:5433`, API `:8000/8001`).

This is not a criticism of the current phase — the in-process seam is the right
first slice — but "works on GCP" is currently unproven in every dimension.
The good news (§1) is that several design choices already point the right way.

References checked: localgcp v0.6.0 docs (slokam-ai/localgcp, localgcp.com/docs),
`googleapis/google-cloud-go` pubsub docs (emulator, Receive, pstest),
`hashicorp/terraform-provider-google` pubsub subscription docs
(dead_letter_policy, retry_policy, push_config OIDC).

## 1. What is already GCP-shaped (keep)

| Asset | Why it transfers |
|---|---|
| Events carry IDs + SHA only (`tenant/claim/document_id/sha256`, `schema_version`) | Pub/Sub 10 MB limit irrelevant; no PII/PHI in transit; filterable by attributes later |
| Idempotent persistence (`ON CONFLICT DO NOTHING`, canonical-ID convergence) | Exactly what at-least-once delivery needs; survives redelivery without distributed locks |
| Versioned event contracts (`document-uploaded.v1`) | Consumer can gate on breaking changes; matches schema-evolution practice |
| slog JSON to stdout, no raw content/secrets in logs | Ingestible by Cloud Logging as-is (severity/trace mapping still needed, §2.5) |
| `Subscriber` port + `StartDocumentWorker` seam | Transport swap is one adapter + one wiring change, no worker rewrite |
| Vanilla postgres SQL (migrations 001–004) | Portable to Cloud SQL Postgres; no exotic extensions |
| `pstest` exists upstream (`pubsub/pstest`) | Unit tests can run against a fake server with no emulator at all |

## 2. Gaps that block "seamless" GCP

### 2.1 No Pub/Sub transport (the big one)

Must add `cloud.google.com/go/pubsub` and an adapter implementing publish behind
`ports.EventBus` and a pull loop (or push endpoint) behind `ports.Subscriber`.
Upstream-confirmed emulator behavior: set `PUBSUB_EMULATOR_HOST=localhost:8085`
(`eval $(localgcp env)`), `pubsub.NewClient(ctx, "any-project")` needs no
credentials and works unchanged — this is directly testable today.

Design decisions to lock before coding:
- **Pull vs push.** Pull (`sub.Receive`, StreamingPull) keeps the current worker
  shape; needs a continuously-running subscriber (Cloud Run service with
  min-instances, or a Run Job). Push (localgcp POSTs to our HTTPS endpoint,
  auto-ack on 2xx) fits Cloud Run serving better but adds an authenticated
  webhook surface (OIDC service-account validation) and changes delivery
  semantics. Recommendation: **pull first** (smaller delta, emulator-provable),
  push only if scale demands it.
- **Topic/subscription topology.** Minimum: `claimops-document-uploaded` topic +
  pull subscription + `claimops-document-uploaded-dlq` topic + DLQ subscription.
  `max_delivery_attempts = 5` server-side.
- **Ordering.** Not needed (per-document idempotency makes order irrelevant).
  Do NOT enable `enable_message_ordering` — it caps throughput for no benefit.

### 2.2 Retry accounting does not survive the transport (correctness!)

`worker.MaxAttempts = 3` is an **in-memory counter**. On real Pub/Sub every
redelivery is a fresh `Handle` call (possibly on a fresh instance), so the
counter resets and a poison message retries forever (or until retention expiry).
Fix: treat Pub/Sub's redelivery as the retry loop —
`max_delivery_attempts = 5` + `dead_letter_policy` in Terraform, nack-and-drop
in code, and move attempt visibility to logs/metrics (log `delivery_attempt`
from the received message attributes). The in-process counter stays only as a
defense for the in-memory bus. The emulator supports DLQ topics — provable locally.

### 2.3 Blob content has no home

`ingest.BlobStore` is process memory: content does not survive restarts, is
invisible to other instances, and unbounded. GCP home is a GCS bucket
(`claimops-documents-<env>`), Storage emulator on `:4443` via
`STORAGE_EMULATOR_HOST`. Change: `ContentFetcher` backed by GCS object reads
(`docID` → object name), upload at ingestion time. Keep the `BlobStore`
as the unit-test fake. Never put content in Pub/Sub messages (works today by
accident of small fixtures — enforce by construction: event struct stays
IDs-only, add a test asserting marshalled event size < 1 KB).

### 2.4 Config and secrets

`config.Load` knows nothing of GCP: no project ID, topic/subscription names,
bucket, emulator hosts. And secrets (DSN today, SA keys tomorrow) are plaintext
env. Changes:
- Add `GCP_PROJECT`, `PUBSUB_TOPIC_DOCUMENT_UPLOADED`,
  `PUBSUB_SUBSCRIPTION_DOCUMENTS`, `GCS_BUCKET_DOCUMENTS` (validated at startup,
  fail fast per repo standard). Emulator hosts pass through (`*_EMULATOR_HOST`
  read by the client libs themselves — do NOT re-implement).
- Secrets via Secret Manager in prod (Go client, manual endpoint for the
  emulator on :8086), env-file only for local. No SA key files in the repo, ever.

### 2.5 Observability mapping

- Cloud Logging: JSON-to-stdout is picked up, but severity must map
  (`slog` levels → `severity` field) and request correlation must use Cloud
  Trace format (`projects/<p>/traces/<trace-id>`) for log↔trace linking.
  Our `correlation_id` should become the trace ID (already the convention —
  just format it).
- Cloud Trace: `observability` OTEL stub needs a real exporter (OTLP) with a
  no-op when disabled; trace the publish→receive→verify span chain.
- Metrics: our registry is in-process Prometheus text; on GCP either keep
  `/metrics` + Managed Service for Prometheus scraping, or emit OpenTelemetry
  metrics. Decide once, not per service.

### 2.6 Runtime packaging (missing entirely)

No container or service definition exists under `infra/`. Needed: minimal
`Dockerfile` (distroless, non-root, `GOMEMLIMIT`), Cloud Run service
(min/max instances, CPU/memory, concurrency = 1 for the pull worker to avoid
duplicate processing races — or >1, since idempotency holds, but start at 1),
service accounts per component (api, worker) with least privilege, and IAM:
`roles/pubsub.publisher` (api → topic), `roles/pubsub.subscriber` (worker → sub),
DLQ ack permission for the Pub/Sub service agent
(`service-<num>@gcp-sa-pubsub.iam.gserviceaccount.com` needs Acknowledge on the
subscription — easy to miss, breaks DLQ silently).

### 2.7 Postgres → Cloud SQL

Migrations are portable, but provisioning is not code: instance, database,
`claimops_app` role, least-privilege grants (re-assert SELECT+INSERT-only on
documents/field_evidence post-provision — drift here silently reopens UPDATE),
Cloud SQL Auth Proxy or private IP + connector in code. Connection pooling
settings (pgxpool max conns) must fit Cloud SQL tier limits.

## 3. Proposed Terraform layout (new `infra/terraform/`)

```text
infra/terraform/
  versions.tf            # required_version, google provider ~> major, backend gcs
  variables.tf           # project, region, env
  apis.tf                # google_project_service: pubsub, storage, sqladmin,
                         #   secretmanager, run, logging, monitoring, eventarc?
  pubsub.tf              # topic + DLQ topic, pull sub (ack 60s, retain 7d,
                         #   retry 10s→600s, dead_letter max 5) + DLQ sub
  storage.tf             # documents bucket (uniform access, versioning off,
                         #   lifecycle: nearline after 90d), no public access
  database.tf            # Cloud SQL postgres, db, claimops_app user, grants
  secrets.tf             # dsn, policy credentials refs (values never in state)
  run.tf                 # api service + worker service, SAs, env, mounts
  iam.tf                 # publisher/subscriber/invoker bindings, DLQ agent ack
  environments/
    local.tfvars         # project=local-dev, emulator hosts, tiny tiers
    prod.tfvars          # real project, retention, min-instances
```

Keep local and prod the **same modules**, different tfvars — that is what makes
localgcp a faithful rehearsal rather than theater.

## 4. Test plan against the running emulator (all doable now)

```bash
eval $(localgcp env)   # PUBSUB_EMULATOR_HOST, STORAGE_EMULATOR_HOST, ...

# 1. Transport adapter (needs cloud.google.com/go/pubsub added):
#    table test with pstest.NewServer (no emulator), then:
PUBSUB_EMULATOR_HOST=localhost:8085 go test ./internal/adapters/pubsub/ -count=1

# 2. DLQ behavior: publish poison event (bad schema_version), assert it lands
#    in the -dlq subscription after max_delivery_attempts (emulator supports this).

# 3. Blob->GCS: STORAGE_EMULATOR_HOST=localhost:4443 go test ./internal/adapters/blobs/

# 4. Full loop on the emulator: API (InMemoryBus -> pubsub publish) ->
#    worker pull -> postgres (local :5433 or --services=cloudsql :5432) ->
#    Mockoon policy. Assert EXTRACTED + evidence + metrics, as in the last E2E.

# 5. `make` targets: `make emulator-test` (starts localgcp --data-dir=.localgcp?
#    or assumes running; runs 1-4), `make tf-plan ENV=local`.
```

## 5. Suggested sequencing (issues)

1. `feat(pubsub)`: transport adapter + config fields + `pstest` unit tests (no emulator needed).
2. `feat(blobs)`: GCS-backed ContentFetcher + 1 KB event-size test.
3. `fix(worker)`: server-side retries — nack semantics, DLQ wiring, drop in-memory
   attempts as the authority (keep as fallback), log delivery_attempt.
4. `chore(obs)`: severity/trace mapping + OTEL exporter behind existing flag.
5. `chore(deploy)`: Dockerfile + `infra/terraform` skeleton (apis/pubsub/storage first,
   run/database after), `local.tfvars` matching the emulator on this machine.
6. `chore(secrets)`: Secret Manager reads with env fallback.
7. E2E on emulator (§4.4) as the merge gate for 1–3; `terraform plan` clean for 5.

Estimated shape, not a commitment: 1–3 are Go-only (days), 4–6 are half-day
each once 1 lands, 7 is the proof. Nothing here requires the AI layer.

## 6. Open questions for review

- Pull vs push for the worker (§2.1)? Default proposed: pull.
- One Cloud Run service (api+worker) or two? Proposed: two, separate SAs.
- Keep Mockoon for policy in local, or stub policy behind an interface already
  (already interfaced — keep Mockoon, note its static tenant in runbooks).
- localgcp `--data-dir` committed to repo or gitignored? Proposed: gitignored,
  recreated by `make emulator-test`.
