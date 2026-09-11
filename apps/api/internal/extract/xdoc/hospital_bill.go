// Hospital bill extractor: the financial record carrying stay context,
// the payable total, and per-row line items from canonical tables. It is
// the only extractor with billLines enabled: line items come from
// reconstructed tables, never from line-like block prose.
package xdoc

import (
	"context"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
)

// hospitalBillKeys is the scalar key set this document type observes.
// Line items arrive separately as the bill_lines_N family.
var hospitalBillKeys = []string{
	KeyPatientName,
	KeyHospitalName,
	KeyAdmissionDate,
	KeyDischargeDate,
	KeyTotalAmountPaise,
}

// HospitalBill extracts canonical facts from hospital-bill artifacts.
type HospitalBill struct{}

// Name is the stable extractor name recorded on every ExtractedField.
func (HospitalBill) Name() string { return "xdoc-hospital-bill" }

// Version is the extractor version for reproducibility.
func (HospitalBill) Version() string { return Version }

// Extract converts doc into canonical facts via the shared label-anchored
// engine with table-backed bill line items. It reports what the artifact
// says, never whether it is right.
func (HospitalBill) Extract(ctx context.Context, doc parser.ParsedDocument) (extract.DocumentFacts, error) {
	return run(ctx, doc, spec{docType: DocHospitalBill, name: HospitalBill{}.Name(), keys: hospitalBillKeys, billLines: true})
}
