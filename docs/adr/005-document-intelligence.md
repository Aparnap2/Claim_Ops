# ADR-005: Document intelligence without OCR infrastructure

Date: 2026-09-06
Status: accepted

## Context

Documents must become evidence before becoming AI context. OCR
infrastructure would couple the domain to heavy native dependencies
and a VPS footprint this project explicitly avoids.

## Decision

1. **Three extraction tiers**: Tier-1 deterministic (embedded text +
   line-regex + normalization, cost 0) → Tier-2 OCR behind the
   `OCRProvider` port (`NoopOCR` fails closed today) → Tier-3 AI
   extraction (Phase 6+, structured candidate fields verified by Go).
2. **Field evidence carries provenance**: every extracted value keeps
   document, page, source anchor, confidence, and extractor version.
   The agent reasons over evidence rows, never raw PDFs.
3. **10-rule verification engine** (pure Go): identity, chronology,
   amounts, completeness, external policy, cross-source contradictions.
   Detection is deterministic; *explanation* is the future agent's job.
4. **Ingestion is idempotent and evented**: SHA-256 dedup on
   `(tenant, claim, sha256)`; `DocumentUploaded` published on an
   `EventBus` port (in-memory today).
5. **Append-only again**: `documents` / `field_evidence` are
   SELECT+INSERT for the service role with RLS FORCE.

## Deferred (tracked, not hidden)

- Issue #10: async worker consumer (classify → extract → normalize →
  verify) behind the bus seam; localgcp Pub/Sub adapter then.
- Real OCR provider implementation; document-blob storage (localgcp
  GCS) — metadata only in this phase.
