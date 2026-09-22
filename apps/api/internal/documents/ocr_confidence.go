package documents

import (
	"errors"
	"math"
)

// OCRConfidence is the typed provider-confidence value object for APA-12.
//
// Invariant: only a trusted, valid provider signal may be treated as
// available. Every unavailable or invalid state fails closed to HITL
// (IsLow == true). The processor must call IsLow(0.85) rather than owning
// the trust policy itself.
//
// Available == false means no trustworthy provider signal (NoopOCR, placeholder
// vendorSilent 1.0/0.85, missing OCR). Available == true means the value came
// from a real provider and passed [0,1] + not-NaN validation at construction.

var ErrInvalidOCRConfidence = errors.New("documents: invalid OCR confidence (want [0,1], not NaN)")

// LowOCRHITLThreshold is the boundary where confidence is insufficient.
// Contract (APA-12): value <= 0.85 -> HITL YES, value > 0.85 -> HITL NO,
// unavailable/invalid -> HITL YES.
const LowOCRHITLThreshold = 0.85

type OCRConfidence struct {
	available bool
	value     float64
}

// NewOCRConfidence constructs an available provider confidence. It validates
// [0,1] and rejects NaN/Inf. Untrusted or missing signals must use
// UnavailableOCRConfidence instead of calling this with a placeholder.
func NewOCRConfidence(v float64) (OCRConfidence, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
		return OCRConfidence{}, ErrInvalidOCRConfidence
	}
	return OCRConfidence{available: true, value: v}, nil
}

// UnavailableOCRConfidence returns the unavailable marker (no trustworthy
// provider signal). It always reports IsLow == true.
func UnavailableOCRConfidence() OCRConfidence {
	return OCRConfidence{available: false, value: 0}
}

// IsAvailable reports whether this is a trusted provider value.
func (c OCRConfidence) IsAvailable() bool { return c.available }

// Value returns the provider value. It panics if unavailable — callers must
// check IsAvailable first. This prevents accidental use of the zero value.
func (c OCRConfidence) Value() float64 {
	if !c.available {
		panic("documents: OCRConfidence Value called on unavailable")
	}
	return c.value
}

// IsLow reports whether the confidence should route to HITL at threshold.
// Unavailable or invalid -> true (fail closed). Available -> value <= threshold.
//
// Note: invalid NaN/outside values cannot be constructed via NewOCRConfidence,
// but if one is somehow present (e.g., direct struct literal in test), IsLow
// still fails closed.
func (c OCRConfidence) IsLow(threshold float64) bool {
	if !c.available {
		return true
	}
	if math.IsNaN(c.value) || math.IsInf(c.value, 0) || c.value < 0 || c.value > 1 {
		return true
	}
	return c.value <= threshold
}
