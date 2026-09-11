// Lab report extractor: the investigation record carrying the patient,
// the facility, and the recorded finding (observed as diagnosis).
// Coverage, stay, amount, and procedure keys are not lab-report concerns.
package xdoc

import (
	"context"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
)

// labReportKeys is the scalar key set this document type observes.
var labReportKeys = []string{
	KeyPatientName,
	KeyHospitalName,
	KeyDiagnosis,
}

// LabReport extracts canonical facts from lab-report artifacts.
type LabReport struct{}

// Name is the stable extractor name recorded on every ExtractedField.
func (LabReport) Name() string { return "xdoc-lab-report" }

// Version is the extractor version for reproducibility.
func (LabReport) Version() string { return Version }

// Extract converts doc into canonical facts via the shared label-anchored
// engine. It reports what the artifact says, never whether it is right.
func (LabReport) Extract(ctx context.Context, doc parser.ParsedDocument) (extract.DocumentFacts, error) {
	return run(ctx, doc, spec{docType: DocLabReport, name: LabReport{}.Name(), keys: labReportKeys})
}
