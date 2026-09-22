package documents

import (
	"math"
	"testing"
)

// APA-12 RED: confidence semantic contract.
// Observable contract, not implementation shape:
//   value < 0.85  -> HITL YES
//   value == 0.85 -> HITL YES
//   value > 0.85  -> HITL NO
//   unavailable/untrusted -> HITL YES
//   NaN / outside [0,1] -> fail closed -> HITL YES
// The processor must expose a simple IsLow(0.85) rather than owning trust policy.

func TestOCRConfidence_IsLow_BelowThreshold(t *testing.T) {
	c, err := NewOCRConfidence(0.84)
	if err != nil {
		t.Fatalf("NewOCRConfidence(0.84): %v", err)
	}
	if !c.IsLow(0.85) {
		t.Fatalf("0.84 IsLow(0.85)=false, want true (HITL YES)")
	}
	if !c.IsAvailable() {
		t.Fatalf("0.84 IsAvailable=false, want true")
	}
}

func TestOCRConfidence_IsLow_AtThreshold(t *testing.T) {
	c, err := NewOCRConfidence(0.85)
	if err != nil {
		t.Fatalf("NewOCRConfidence(0.85): %v", err)
	}
	if !c.IsLow(0.85) {
		t.Fatalf("0.85 IsLow(0.85)=false, want true (boundary HITL YES)")
	}
}

func TestOCRConfidence_IsLow_AboveThreshold(t *testing.T) {
	c, err := NewOCRConfidence(0.86)
	if err != nil {
		t.Fatalf("NewOCRConfidence(0.86): %v", err)
	}
	if c.IsLow(0.85) {
		t.Fatalf("0.86 IsLow(0.85)=true, want false (HITL NO)")
	}
}

func TestOCRConfidence_Unavailable_IsLow(t *testing.T) {
	c := UnavailableOCRConfidence()
	if c.IsAvailable() {
		t.Fatalf("Unavailable IsAvailable=true, want false")
	}
	if !c.IsLow(0.85) {
		t.Fatalf("Unavailable IsLow=false, want true (HITL YES, no trusted signal)")
	}
	// Must not panic on IsLow when unavailable
}

func TestOCRConfidence_Invalid_NaN_FailClosed(t *testing.T) {
	_, err := NewOCRConfidence(math.NaN())
	if err == nil {
		t.Fatalf("NaN NewOCRConfidence should fail closed (invalid)")
	}
}

func TestOCRConfidence_Invalid_OutsideRange_FailClosed(t *testing.T) {
	for _, v := range []float64{-0.1, 1.5, 2.0} {
		_, err := NewOCRConfidence(v)
		if err == nil {
			t.Fatalf("NewOCRConfidence(%v) should fail closed (outside [0,1])", v)
		}
	}
}

func TestOCRConfidence_ClassifyPlaceholder_NotProviderConfidence(t *testing.T) {
	// documents.Classify returns 0.85 for HOSPITAL_BILL — deterministic classification
	// confidence, NOT provider OCR confidence. It must not be treated as available
	// provider signal merely because it is numerically valid.
	_, conf := Classify("hospital_bill_final.pdf", "application/pdf")
	if conf != 0.85 {
		t.Fatalf("Classify bill conf=%v, want 0.85", conf)
	}
	// The OCR seam must not be constructed from classification output.
	// Only provider-trusted construction (NewOCRConfidence called with real OCR
	// provider value) yields available=true. For classification-driven paths,
	// the gate must use UnavailableOCRConfidence, which is low -> HITL YES.
	c := UnavailableOCRConfidence()
	if c.IsAvailable() {
		t.Fatalf("Classify-derived must be unavailable")
	}
	if !c.IsLow(0.85) {
		t.Fatalf("Classify-derived unavailable IsLow=false, want true (HITL YES)")
	}
}
