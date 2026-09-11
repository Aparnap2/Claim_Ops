# ClaimOps — Library-Specific Patterns

## Go 1.27
- `go.mod` targets Go 1.27.
- Keep byte-sensitive contracts on the existing serialization path unless an explicit migration is approved.
- `encoding/json/v2` was evaluated and rejected for byte-sensitive outbox serialization because marshaling was not byte-identical under the required contract.
- Use `errors.Is`/`errors.As`, context cancellation, structured logging, and typed domain errors.
- Use `go test`, `go vet`, and race testing as appropriate.

## Fiber
- Keep handlers thin.
- Validate tenant headers and request schemas at the edge.
- Preserve X-Request-ID behavior.
- Use the established error envelope and stable error codes.
- Do not put database transactions or long business workflows directly into handlers.

## pgx/PostgreSQL
- PostgreSQL is authoritative.
- Use the established tenant transaction helper.
- Preserve transaction-local tenant configuration for RLS.
- Never query tenant data outside the intended RLS context.
- Preserve optimistic version checks.
- Use savepoints where required by established idempotent append behavior.
- Treat audit tables as append-only.

## GCS / BlobStore
- Use the BlobStore port, not direct GCS calls from domain code.
- Managed objects follow the established claim/document object path convention.
- Verify SHA-256 integrity before parsing.
- Bound reads.
- Treat blob deletion as observable and idempotent.
- Never log document bytes.

## Pub/Sub
- Events are IDs-only and intentionally small.
- Server-side delivery retry/DLQ configuration owns production retry accounting.
- Consumers must be idempotent.
- Do not place document contents into messages.
- Preserve event contract/version semantics.

## localgcp
Local development uses the project's existing localgcp environment. Do not invent ports or replacement emulators; inspect repository configuration.
Known local services include Pub/Sub, GCS/storage, Firestore, Cloud Tasks, Logging, Secret Manager, KMS, Cloud Run, and Vertex AI.

## LiteParse
- Version is pinned in `uv.lock`.
- Access it only through `internal/parser/liteparse`.
- Do not leak LiteParse vendor types outside the adapter.
- Input must be `parser.TrustedDocument`.
- Output must be the canonical parser artifact.
- OCR is intentionally off in the current adapter.
- Corrupt/raster/blank cases must not cause adapter crashes.
- Do not fabricate confidence or provenance.
- Do not add production routing logic inside the adapter.

## Python / ADK
- Python is the bounded cognitive service, not the system of record.
- LLMs operate on constrained evidence and tools.
- Never allow an agent to directly approve/deny/pay/change policy/mutate authoritative state.
- Prefer structured outputs and explicit tool capabilities.
- Keep deterministic preconditions outside the LLM.

## Mockoon
- Use existing Mockoon contracts for external/legacy insurance APIs.
- Keep external schemas behind adapters.
- Tests must cover missing keys, tenant mismatch, malformed responses, and expected error conditions.
- Never make production code depend on Mockoon-specific behavior.

## General dependency rule
Before adding a library:
1. Check whether the repository already has an abstraction.
2. Check whether the standard library is sufficient.
3. Check licensing and operational footprint.
4. Pin the dependency.
5. Add tests proving the intended behavior.
