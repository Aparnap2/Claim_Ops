# ClaimOps — Decision Log

## Purpose
This is a compact index of architectural decisions and important implementation choices. Detailed ADRs remain authoritative where they exist.

## Current decisions

### Deterministic-first architecture
Deterministic software owns truth, state, control, validation, and side effects. AI is bounded to ambiguity/cognition.

Reason:
- easier testing
- reproducibility
- fault localization
- safer automation
- narrower AI evaluation surface

### Go authoritative backend
Go/Fiber is the authoritative backend/system-of-record boundary.

Reason:
- strong typing
- concurrency/runtime suitability
- explicit contracts
- operational simplicity

### Python/ADK bounded cognitive service
Python is reserved for cognitive/AI workloads and is not the system of record.

### PostgreSQL + RLS
PostgreSQL is authoritative with tenant isolation enforced through RLS/FORCE RLS.

### IDs-only events
Pub/Sub events contain identifiers and hashes rather than document contents.

Reason:
- small messages
- lower leakage risk
- stable event contracts
- document data remains behind controlled storage access

### Document trust boundary
Untrusted input must pass admission and integrity verification before parsing.

### Evidence-first AI
The future investigation agent reasons over structured evidence and verified findings, not arbitrary raw documents whenever avoidable.

### LiteParse-only parser for current stage
LiteParse is the current parser adapter.

Docling was deliberately dropped from #30 because its installation/model-download/operational footprint was impractical for the current environment.

This is not a permanent statement that Docling is inferior.

### OCR is an escalation capability
OCR is currently disabled in the LiteParse adapter.

Managed GCP OCR may be added later if benchmark evidence demonstrates the need and operational/cost/privacy constraints are acceptable.

Do not make free-tier availability an architectural dependency.

### Parser contract is vendor-neutral
`internal/parser` owns:
- TrustedDocument
- canonical artifacts
- parser interface
- parser error taxonomy
- conformance

Vendor types remain inside adapters.

### Corpus is synthetic/reproducible
The parser benchmark corpus contains no real patient PII/PHI.
Synthetic documents are the primary reproducible benchmark source.

### JSON v2
Go 1.27 was adopted.
`encoding/json/v2` was evaluated and rejected for byte-sensitive outbox serialization because marshaling was not byte-identical under the existing contract.

This does not prohibit future use in non-byte-sensitive contexts after explicit review.

### Deterministic claim chain #44–#47 (complete)
Extraction observation (#44/#45, no confidence floats) -> canonical assembly (#46, agreement/ConflictEntry/NeedsReview) -> verification R1–R10 + Unresolved channel via verifywrap (#47). Investigation input (UnresolvedException) is specified only; no agent implementation exists.

## Decided: #33 parser/OCR/routing policy (ADR-007, accepted 2026-09-11)
#33 selected the parser/OCR/routing policy using measured evidence from #31/#32: LiteParse 2.14.4 default (OCR off), deterministic sufficiency gate, managed OCR optional on measured evidence. Detail: docs/adr/007-parser-policy.md. The pre-decision options considered were:

Options considered (historical):
- LiteParse sufficient as-is
- LiteParse + managed OCR escalation
- additional parser/capability required
- corpus expansion required before production decision

The decision was evidence-driven.

### #32 evidence (LiteParse 2.14.4, 45 cases, deterministic)
- 45/45 parse_ok. Fields 280 exact / 27 normalized / 150 missing. Tables 27 pass / 13 partial / 3 missed.
- D0 strong; D6/D7 collapse with EMPTY_ARTIFACT (OCR off by design).
- Table cells carry no block-id by contract (unavailable-by-design, not parser failure).
- Vendor-silent confidence 1.0 is uncalibrated; never rewarded by the scorer.
- Full evidence: docs/evaluations/parser/v1-report.md. Input to #33 recorded as questions, not decisions.
