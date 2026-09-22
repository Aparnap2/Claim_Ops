package worker

import (
	"strings"

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

// isConfidenceValidationError reports whether a ParsedDocument.Validate error
// is due to confidence (APA-12). Invalid confidence is evidence-quality (HITL),
// not parser integrity, so the worker must route it to the low-OCR gate rather
// than terminal FAILED.
func isConfidenceValidationError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "confidence must be in [0,1]")
}

// aggregateOCRConfidence collapses parsed blocks into a single OCRConfidence
// for the HITL gate. Only blocks with ConfidenceAvailable=true are treated
// as provider measurements; placeholder blocks (LiteParse vendorSilent 1.0
// with ConfidenceAvailable=false) collapse to Unavailable (fail closed to
// HITL). This enforces the invariant that only a trusted provider signal
// may become available=true. Empty documents (no blocks) are unavailable.
func aggregateOCRConfidence(doc parser.ParsedDocument) documents.OCRConfidence {
	if len(doc.Pages) == 0 {
		return documents.UnavailableOCRConfidence()
	}
	hasBlock := false
	for _, p := range doc.Pages {
		for _, b := range p.Blocks {
			hasBlock = true
			if !b.ConfidenceAvailable {
				return documents.UnavailableOCRConfidence()
			}
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
	// All blocks high -> aggregate high.
	c, _ := documents.NewOCRConfidence(1.0)
	return c
}
