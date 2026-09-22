# ADR 009 — OCR Confidence Semantics and Low-Evidence HITL Gate

* Status: Accepted 2026-09-22 (APA-12)
* Base: `main@10d8256` after APA-11
* Context: audit found Real OCR returns constant Confidence=0.85, gate is confidence < 0.85, so successful OCR never triggers low-confidence path. Placeholder confidences (LiteParse vendorSilent 1.0, classification 0.85) are not provider measurements.

## Decision

**Typed value object `documents.OCRConfidence` is the sole confidence carrier.**

* `NewOCRConfidence(v)` validates `[0,1]` and rejects NaN/Inf; unavailable signals use `UnavailableOCRConfidence()` (available=false). No bare `float64` is ever treated as trustworthy.
* `IsLow(0.85)` is the HITL predicate: unavailable/invalid -> true, value <= 0.85 -> true, value > 0.85 -> false. Threshold inclusive of boundary (APA-12 contract: 0.85 itself is low).
* Classification confidence (`Classify` returns 0.85 for HOSPITAL_BILL) is **not** provider OCR confidence and must never be wrapped as available; processor gate uses only parser-block aggregate, not `Classify` output.
* `parser.ContentBlock` now carries `ConfidenceAvailable bool` provenance: only `ConfidenceAvailable=true` blocks are provider measurements that may become `available=true` via `NewOCRConfidence`; `false` (LiteParse vendorSilent 1.0, classification 0.85, missing) is placeholder/unavailable. LiteParse `vendorSilentConfidence=1.0` is now stamped with `ConfidenceAvailable=false` so the worker aggregate treats it as `Unavailable` -> low -> HITL, enforcing that only a trusted provider signal may become available. No bare `float64` is ever treated as trustworthy.

**Deterministic HITL gate:** `worker.shouldEscalateForOCR` + `aggregateOCRConfidence(parsed)` in `worker.Processor.runNewPipeline`. Gate is evaluated after `Parse` and `Validate` (with `isConfidenceValidationError` routing invalid confidence to HITL rather than terminal `FAILED`), before extraction. Low OCR (unavailable, invalid NaN/outside, or value <=0.85) appends `LOW_OCR_CONFIDENCE` to `Outcome.ExceptionCodes` and synthesizes an R8 envelope so low-quality evidence reaches HITL/exception handling with tenant/claim scoping preserved. Tier-1 regex path (no Parser) is unaffected.

## Consequences

* Below / equal / above 0.85 and unavailable/invalid are all deterministically tested (`documents/ocr_confidence_test.go` 7 tests, `worker/processor_ocr_test.go` 8 tests including full-chain integration).
* Placeholder 0.85 no longer silently passes the gate; unavailable never becomes high confidence.
* No `eval-v1` change; verify taxonomy stays closed (R1-R10), low OCR reuses R8 envelope for HITL routing.
* Business behavior: low OCR docs now correctly route to HITL instead of silent success; high OCR docs unchanged.

## Alternatives

* Bare `float64` + `bool` available flag at call sites — rejected, semantics would leak and the `<` vs `<=` boundary would be re-introduced at each site.

