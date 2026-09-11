// Policy schedule extractor: the coverage record carrying the policy
// number and the insured person (observed as patient_name, the identity
// key verify R2 compares). Stay, amount, and clinical keys are not
// policy-schedule concerns.
package xdoc

import (
	"context"

	"claimops-api/internal/extract"
	"claimops-api/internal/parser"
)

// policyScheduleKeys is the scalar key set this document type observes.
var policyScheduleKeys = []string{
	KeyPolicyNumber,
	KeyPatientName,
}

// PolicySchedule extracts canonical facts from policy-schedule artifacts.
type PolicySchedule struct{}

// Name is the stable extractor name recorded on every ExtractedField.
func (PolicySchedule) Name() string { return "xdoc-policy-schedule" }

// Version is the extractor version for reproducibility.
func (PolicySchedule) Version() string { return Version }

// Extract converts doc into canonical facts via the shared label-anchored
// engine. It reports what the artifact says, never whether it is right.
func (PolicySchedule) Extract(ctx context.Context, doc parser.ParsedDocument) (extract.DocumentFacts, error) {
	return run(ctx, doc, spec{docType: DocPolicySchedule, name: PolicySchedule{}.Name(), keys: policyScheduleKeys})
}
