# ClaimOps — Agent Operating Notes

## Source of truth (read every session)
`docs/specs/` is project memory. Read before changing code:

| File | Purpose |
|------|---------|
| 01_CODE_STANDARDS.md | Engineering standard (observability, metrics, contracts, gates) |
| 02_UI_RULES.md | UI conventions |
| 03_DESIGN_TOKENS.md | Design tokens |
| 04_LIBRARY_PATTERNS.md | Approved library usage |
| 05_BUILD_PLAN.md | Milestone chain and sequencing |
| 06_PROGRESS.md | Current state, completed proofs, next work |
| 07_PROJECT_CONTEXT.md | What ClaimOps is / is not |
| 08_AGENT_WORKFLOW.md | How to work (issue → inspect → plan → TDD → verify → PR) |
| 09_DECISION_LOG.md | Locked decisions (update on every merged issue) |
| `docs/KNOWN_LIMITATIONS.md` | Release-relevant known limitations, each row evidenced (read before claiming qualification) |

Hierarchy on conflict: GitHub issue → ADRs/contracts → docs/specs → tests/fixtures → judgment.
If sources conflict, STOP and report. Never silently choose.

## Architecture (do not drift)
- Go/Fiber authoritative backend (`apps/api`); Go application-owned cognitive orchestration (`internal/investigate` + provider-neutral ModelClient); Python is spec artifacts + bounded future service per ADR-001
- Postgres authoritative; GCS blobs; Pub/Sub transport; Mockoon externals; localgcp local parity
- Deterministic where truth is knowable; cognitive where interpretation is necessary; human authority where consequences require judgment
- Parser vendor types never cross `internal/parser`. Corpus changes require requalification.
- No real PII/PHI in repo, corpus, CI artifacts, or history. Ever.

## Gates (every change)
`gofmt` clean · `go vet` clean · `go test ./...` green (live infra: PG :5433, localgcp :8085/:4443, Mockoon :3001) · `uv run pytest tests/unit` green · `ruff` clean on touched Python.

## Current chain
#28 contract ✓ → #29 corpus ✓ → #30 LiteParse adapter ✓ → #31 benchmark ✓ → #32 report ✓ → #33 ADR ✓ → #44-#47 deterministic chain ✓ → #50 ordering ✓ → #53 contracts ✓ → #54 tools ✓ → #66 orchestration ✓ → eval v1 ✓ → #80 GCW/Agent/ModelClients slices 1-2 ✓ → APA-9 HMAC ✓ → APA-10 pending-state ✓ → APA-11/ADR-008 ✓ → APA-12/ADR-009 ✓ → APA-13 runtime bounds ✓ → APA-31 sufficiency gate ✓ → APA-34–APA-45 Phase-3 slices ✓ → APA-48 worker parse runtime ✓ → APA-49/50/54/56/59 prompt-contract chain ✓ → APA-64 ✓ (main@dffa160). Blocked on upstream/credentials per `docs/KNOWN_LIMITATIONS.md`; APA-37 and APA-40 adoption are NOT locally qualified.
Go 1.27 pinned. `encoding/json/v2` rejected for byte-sensitive paths (see 09_DECISION_LOG.md). Groq model is `qwen/qwen3.8-27b` (ADR-002 + code default); `llama-3.1-8b-instant` is retired and superseded — see 09_DECISION_LOG.md.
