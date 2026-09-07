# Dataset A — deterministic engineering corpus (synthetic)

Purpose: pipeline and failure-injection tests. These inputs say NOTHING
about parser quality — that is Dataset B (real PDFs, tracked in issue #18,
blocked on corpus acquisition).

Cases (generated in-code by `internal/ingest/corpus_test.go`, no binaries
committed):

| Case | Bytes | Expectation |
|---|---|---|
| `empty` | 0 bytes | validation error, zero side effects (no blob, no row, no outbox) |
| `garbage-as-pdf` | text bytes, MIME application/pdf | created — transport is bytes-opaque, never judges content |
| `zeros-5m` | 5 MiB zeros | created — no size cap in #15 (tracked: upload limits) |
| `duplicate` | same bytes twice | second Upload returns created=false, single doc row + single outbox row |
| `blob-loss` | staged then deleted pre-fetch | worker fetch fails transient (covered in worker tests) |

Failure-injection coverage lives with the component that owns the failure:
dispatcher crash-replay (`outbox` tests), Nack redelivery (`pubsubadapter`
tests), GCS failure (`ingest/service_test.go`), cross-restart convergence
(`worker` tests), emulator restarts (manual runbook below).

Manual emulator-restart drill:
1. `POST` a document (201, outbox row unpublished).
2. Kill localgcp (`pkill localgcp`), restart `localgcp up` (in-memory state lost).
3. Re-`POST` identical bytes → converges via DB unique constraints; no orphans.
4. Dispatcher re-claims the surviving outbox row on next poll.
