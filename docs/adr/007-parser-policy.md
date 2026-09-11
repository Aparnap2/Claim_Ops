# ADR-007: Production parser policy — LiteParse default with deterministic sufficiency gate

Date: 2026-09-11
Status: accepted

## Context

#28–#32 built an evidence chain, not an opinion: canonical parser
contract (#28), 45-case synthetic corpus with goldens (#29), LiteParse
2.14.4 adapter behind the contract (#30), deterministic benchmark (#31),
and triaged evidence report (#32, docs/evaluations/parser/v1-report.md).

Docling was dropped in #30 (install/model-download footprint
impractical here); the report contains no Docling data and makes no
comparative claim. Managed GCP OCR exists only as a future capability.

## What the benchmark established (and did not)

Established, on the controlled corpus (repeat_equal: true):

- 45/45 parse completion; artifact validity 45/45.
- Fields: 280 exact / 27 normalized / 150 missing (no incorrect class —
  text search proves presence, never wrongness).
- Tables: 27 pass / 13 partial / 3 missed. Flattened-to-paragraphs
  bills are TABLE_MISSED, never hidden in text accuracy.
- Strong D0 → collapse in D6/D7 (EMPTY_ARTIFACT with OCR off, by design).
- 7 reading-order violations (Y0-monotonicity check only).
- Table cells carry no block-id BY CONTRACT DESIGN
  (unavailable-by-design, not parser failure).
- Vendor-silent confidence 1.0 is uncalibrated and never rewarded.

NOT established: universal sufficiency for Indian health-insurance
claims. Proven is "this implementation behaves like this on this
controlled corpus" — not "LiteParse processes all claim documents."

## Decision

1. **Default production parser: LiteParse 2.14.4** behind
   `internal/parser`, OCR off, pinned in uv.lock. No change to the
   adapter, contract, or corpus.
2. **Deterministic sufficiency gate, not premature OCR**: after parsing,
   deterministic validation decides sufficient vs insufficient per
   document (required fields present, tables reconstructed where the
   golden class demands them, artifact valid). Sufficient → continue
   flow. Insufficient → EXCEPTION (deterministic state + evidence
   pointers), routable to a future OCR capability.
3. **OCR stays optional**: managed OCR is NOT mandatory infrastructure.
   It becomes justified only for the measured collapse tiers (D6/D7,
   content-absent classes) and enters behind an application-owned
   interface when built — never as a bypass around admission,
   integrity, or the parser contract.
4. **No second parser until evidence demands one**: Docling (or any
   alternative) re-enters only via the same contract + corpus +
   benchmark path that qualified LiteParse.

## Consequences

- Production needs the sufficiency-gate rules defined (which fields per
  document class, which table classes mandatory) — follow-up work,
  deterministic, testable.
- The exception path from insufficient parses must land in the
  claim workflow (states, ownership, retry) — follow-up work.
- Vendor-silent confidence must be calibrated or replaced before any
  routing logic consumes it.
- Corpus additions require benchmark requalification (locked policy).
