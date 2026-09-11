// Pre-authorization form extractor: the pre-stay request carrying
// identity, coverage, facility, expected amount, and clinical keys. There
// is no discharge date at pre-authorization time, so that key is out of
// scope here.
package xdoc

import (
	"context"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
)

// preauthFormKeys is the scalar key set this document type observes.
var preauthFormKeys = []string{
	KeyClaimNumber,
	KeyPolicyNumber,
	KeyPatientName,
	KeyHospitalName,
	KeyAdmissionDate,
	KeyTotalAmountPaise,
	KeyDiagnosis,
	KeyProcedure,
}

// PreauthForm extracts canonical facts from pre-authorization artifacts.
type PreauthForm struct{}

// Name is the stable extractor name recorded on every ExtractedField.
func (PreauthForm) Name() string { return "xdoc-preauth-form" }

// Version is the extractor version for reproducibility.
func (PreauthForm) Version() string { return Version }

// Extract converts doc into canonical facts via the shared label-anchored
// engine. It reports what the artifact says, never whether it is right.
func (PreauthForm) Extract(ctx context.Context, doc parser.ParsedDocument) (extract.DocumentFacts, error) {
	return run(ctx, doc, spec{docType: DocPreauthForm, name: PreauthForm{}.Name(), keys: preauthFormKeys})
}
