# ADR-006: Push worker for production, pull worker for local

Date: 2026-09-07
Status: accepted

## Context

#15 proved the pipeline on a pull loop (in-process dispatcher + StreamingPull
against localgcp). A permanently-running pull loop inside Cloud Run is
wasteful (min-instances billing) and fights the platform's request-driven
model. PRD §34–§35 require at-least-once delivery, idempotent consumers,
retry with backoff, and DLQ → manual review.

## Decision

1. **Production: Pub/Sub push → `POST /events/document-ingested`** on a
   dedicated `cmd/worker` Cloud Run service. 2xx = ack, anything else =
   redeliver; `max_delivery_attempts = 5` + dead-letter topic server-side.
2. **Local: pull loop stays** (localgcp StreamingPull, same `Processor`).
   Push is emulatable too, but pull exercises the identical application
   contract with less local moving parts.
3. **One application contract both sides**: raw event bytes →
   `HandleDocumentIngested`-equivalent (`app.DocumentEventHandler`), tenant
   scoped per message, always-terminal outcomes, idempotency on
   (tenant, claim, event_id) + canonical document IDs + idempotent inserts.
4. **Push auth**: OIDC-validated in prod (audience = worker URL, email =
   push invoker SA); `none` mode exists for local only and is rejected when
   `APP_ENV=prod`.
5. **Three runtime identities**: api (GCS RW scoped objects, Cloud SQL,
   secrets read, publish via dispatcher), dispatcher = api identity
   (Cloud SQL + `roles/pubsub.publisher`), worker (GCS read, Cloud SQL,
   secrets read; no publish, no admin).
6. **Terraform covers used resources only**: APIs, Artifact Registry,
   Pub/Sub (topic + push sub + DLQ), Storage bucket, Cloud SQL, Secret
   Manager refs, 2× Cloud Run, SAs + bindings. No Firestore/Workflows/Redis/
   GKE/Vertex in this phase.

## Compatibility matrix (must stay green)

| Capability   | Unit fake | Local process | Container | localgcp | GCP |
|---|---|---|---|---|---|
| Postgres     | repo fakes | :5433 | :5433 | :5433 (or cloudsql svc) | Cloud SQL |
| Blob storage | memblob | memblob/GCS emu | GCS emu | GCS emu :4443 | GCS |
| Event bus    | InMemoryBus | InMemoryBus | InMemoryBus | Pub/Sub emu :8085 | Pub/Sub |
| API serving  | httptest | go run | image + /healthz | image | Cloud Run |
| Worker delivery | direct Handle | pull loop | pull loop | pull loop | push endpoint |
| Contract under test | envelope + idempotency | same | same | same | same |

## Consequences

- `cmd/worker` binary + Dockerfiles + `infra/terraform` are new surface to
  maintain; local default path (pull) must not rot — `make localgcp-test`
  covers it.
- Real-GCP `plan`/`apply`/smoke needs a project + billing; until then the
  gate is `terraform fmt + validate` + container parity + emulator E2E.
- DLQ service-agent IAM (`service-<n>@gcp-sa-pubsub`) is called out in
  `iam.tf`; misconfiguring it fails silently in prod — smoke test must
  publish a poison event and watch it land in DLQ.
