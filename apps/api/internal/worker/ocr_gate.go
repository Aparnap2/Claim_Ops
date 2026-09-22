package worker

import (
	"claimops-api/internal/documents"
	"claimops-api/internal/parser"
)

// shouldEscalateForOCR is the deterministic HITL gate for OCR confidence.
// It is the only place that knows the threshold; callers pass the typed
// OCRConfidence and get a boolean. Unavailable/invalid -> true (fail closed
// to HITL), value <= 0.85 -> true, value > 0.85 -> false.
func shouldEscalateForOCR(c documents.OCRConfidence) bool {
	return c.IsLow(documents.LowOCRHITLThreshold)
}

// aggregateOCRConfidence collapses parsed blocks into a single OCRConfidence
// for the HITL gate. LiteParse's vendorSilent 1.0 is a placeholder (not a
// measurement) and is intentionally treated as available 1.0 for now so
// Tier-1/full-chain docs do not spuriously escalate; the scorer never
// rewards it and ADR-007 notes it is uncalibrated. Real OCR providers must
// construct available confidences via documents.NewOCRConfidence; unavailable
// or invalid blocks collapse to Unavailable (fail closed to HITL). Empty
// documents (no blocks) are unavailable.
func aggregateOCRConfidence(doc parser.ParsedDocument) documents.OCRConfidence {
	if len(doc.Pages) == 0 {
		return documents.UnavailableOCRConfidence()
	}
	// If any block is unavailable/invalid/low, the document is low.
	hasBlock := false
	for _, p := range doc.Pages {
		for _, b := range p.Blocks {
			hasBlock = true
			// Placeholder path: LiteParse stamps 1.0 because vendor exposes no
			// confidence. Keep it as available 1.0 so existing docs don't all
			// route to HITL; real providers must supply measured confidence.
			c, err := documents.NewOCRConfidence(b.Confidence)
			if err != nil {
				return documents.UnavailableOCRConfidence()
			}
			if c.IsLow(documents.LowOCRHITLThreshold) {
				return c
			}
		}
	}
	if !hasBlock {
		return documents.UnavailableOCRConfidence()
	}
	// All blocks high -> minimal high confidence (1.0) as aggregate.
	c, _ := documents.NewOCRConfidence(1.0)
	return c
}
