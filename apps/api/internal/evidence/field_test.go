// Pure unit tests for FieldEvidence: no network, no I/O, no clock reads.
// Covers NewFieldEvidence validation rejections, the happy-path field
// mapping, and ID shape/uniqueness.
package evidence_test

import (
	"math"
	"strings"
	"testing"

	"claimops-api/internal/evidence"
)

func TestNewFieldEvidenceRejects(t *testing.T) {
	cases := []struct {
		name       string
		tenant     string
		claim      string
		document   string
		field      string
		extractor  string
		page       int
		confidence float64
	}{
		{name: "blank tenant", tenant: "", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "whitespace tenant", tenant: "   ", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "blank claim", tenant: "tenant-a", claim: "", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "whitespace claim", tenant: "tenant-a", claim: "  ", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "blank document", tenant: "tenant-a", claim: "CLM-001", document: "", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "whitespace document", tenant: "tenant-a", claim: "CLM-001", document: "  ", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "blank field", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "whitespace field", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "  ", extractor: "tier1-regex", page: 1, confidence: 0.9},
		{name: "blank extractor", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "", page: 1, confidence: 0.9},
		{name: "whitespace extractor", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "  ", page: 1, confidence: 0.9},
		{name: "confidence negative", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: -0.1},
		{name: "confidence above one", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: 1.1},
		{name: "confidence NaN", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 1, confidence: math.NaN()},
		{name: "page zero", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: 0, confidence: 0.9},
		{name: "page negative", tenant: "tenant-a", claim: "CLM-001", document: "DOC-001", field: "patient_name", extractor: "tier1-regex", page: -1, confidence: 0.9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := evidence.NewFieldEvidence(tc.tenant, tc.claim, tc.document, "DISCHARGE_SUMMARY",
				tc.field, "Aparna Pradhan", "Patient Name: Aparna Pradhan", tc.extractor, tc.page, tc.confidence)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
		})
	}
}

func TestNewFieldEvidenceOK(t *testing.T) {
	got, err := evidence.NewFieldEvidence("tenant-a", "CLM-001", "DOC-001", "DISCHARGE_SUMMARY",
		"patient_name", "Aparna Pradhan", "Patient Name: Aparna Pradhan", "tier1-regex", 1, 0.9)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(got.ID, "evf-") {
		t.Fatalf("want ID with evf- prefix, got %q", got.ID)
	}
	if len(got.ID) != len("evf-")+16 {
		t.Fatalf("want ID of length %d, got %q", len("evf-")+16, got.ID)
	}
	if string(got.Tenant) != "tenant-a" || string(got.ClaimID) != "CLM-001" {
		t.Fatalf("want tenant tenant-a and claim CLM-001, got %q and %q", got.Tenant, got.ClaimID)
	}
	if got.DocumentID != "DOC-001" || got.Field != "patient_name" || got.Value != "Aparna Pradhan" {
		t.Fatalf("unexpected field mapping: %+v", got)
	}
	if got.Extractor != "tier1-regex" || got.DocType != "DISCHARGE_SUMMARY" {
		t.Fatalf("unexpected extractor/doctype: %+v", got)
	}
	if got.Page != 1 || got.Confidence != 0.9 {
		t.Fatalf("unexpected page/confidence: %+v", got)
	}
}

func TestNewFieldEvidenceConfidenceBounds(t *testing.T) {
	for _, conf := range []float64{0.0, 1.0} {
		if _, err := evidence.NewFieldEvidence("tenant-a", "CLM-001", "DOC-001", "CLAIM_FORM",
			"claim_number", "CLM-001", "Claim No: CLM-001", "tier1-regex", 2, conf); err != nil {
			t.Fatalf("confidence %v: unexpected error: %v", conf, err)
		}
	}
}

func TestNewFieldEvidenceDistinctIDs(t *testing.T) {
	mk := func() evidence.FieldEvidence {
		got, err := evidence.NewFieldEvidence("tenant-a", "CLM-001", "DOC-001", "CLAIM_FORM",
			"claim_number", "CLM-001", "Claim No: CLM-001", "tier1-regex", 1, 0.9)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return got
	}
	first, second := mk(), mk()
	if first.ID == "" || second.ID == "" {
		t.Fatalf("want non-blank IDs, got %q and %q", first.ID, second.ID)
	}
	if first.ID == second.ID {
		t.Fatalf("want distinct IDs, both were %q", first.ID)
	}
}
