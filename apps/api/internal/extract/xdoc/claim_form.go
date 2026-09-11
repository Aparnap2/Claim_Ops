// Claim form extractor: the admission-time request carrying identity,
// coverage, stay, amount, and clinical keys.
package xdoc

import (
	"context"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
)

// claimFormKeys is the scalar key set this document type observes. Bill
// line items are not a claim-form concern (they live on the hospital bill).
var claimFormKeys = []string{
	KeyClaimNumber,
	KeyPolicyNumber,
	KeyPatientName,
	KeyHospitalName,
	KeyAdmissionDate,
	KeyDischargeDate,
	KeyTotalAmountPaise,
	KeyDiagnosis,
	KeyProcedure,
}

// ClaimForm extracts canonical facts from claim-form artifacts.
type ClaimForm struct{}

// Name is the stable extractor name recorded on every ExtractedField.
func (ClaimForm) Name() string { return "xdoc-claim-form" }

// Version is the extractor version for reproducibility.
func (ClaimForm) Version() string { return Version }

// Extract converts doc into canonical facts via the shared label-anchored
// engine. It reports what the artifact says, never whether it is right.
func (ClaimForm) Extract(ctx context.Context, doc parser.ParsedDocument) (extract.DocumentFacts, error) {
	return run(ctx, doc, spec{docType: DocClaimForm, name: ClaimForm{}.Name(), keys: claimFormKeys})
}
