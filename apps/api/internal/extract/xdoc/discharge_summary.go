// Discharge summary extractor: the clinical stay record carrying who,
// where, when, and what was found and done. Coverage and amount keys are
// not discharge-summary concerns.
package xdoc

import (
	"context"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
)

// dischargeSummaryKeys is the scalar key set this document type observes.
var dischargeSummaryKeys = []string{
	KeyPatientName,
	KeyHospitalName,
	KeyAdmissionDate,
	KeyDischargeDate,
	KeyDiagnosis,
	KeyProcedure,
}

// DischargeSummary extracts canonical facts from discharge-summary artifacts.
type DischargeSummary struct{}

// Name is the stable extractor name recorded on every ExtractedField.
func (DischargeSummary) Name() string { return "xdoc-discharge-summary" }

// Version is the extractor version for reproducibility.
func (DischargeSummary) Version() string { return Version }

// Extract converts doc into canonical facts via the shared label-anchored
// engine. It reports what the artifact says, never whether it is right.
func (DischargeSummary) Extract(ctx context.Context, doc parser.ParsedDocument) (extract.DocumentFacts, error) {
	return run(ctx, doc, spec{docType: DocDischargeSummary, name: DischargeSummary{}.Name(), keys: dischargeSummaryKeys})
}
