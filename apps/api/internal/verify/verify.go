// Package verify implements the deterministic claim verification core.
//
// It is stdlib-only by design: no HTTP, no I/O, no logging, no clock reads.
// All money is exact int64 paise; date normalization is assumed pre-done
// (DocEvidence admission_date values are already YYYY-MM-DD).
// All functions are pure. Rule order R1-R10 is the emission order.
package verify

import (
	"fmt"
	"strings"
	"time"
)

// Severity levels for exceptions.
const (
	// SeverityHigh blocks the claim.
	SeverityHigh = "HIGH"
	// SeverityMedium flags a non-blocking concern.
	SeverityMedium = "MEDIUM"
	// SeverityLow is informational.
	SeverityLow = "LOW"
)

// Exception code constants, one per rule.
const (
	// CodePolicyNumberConflict is R1: claim vs policy number mismatch.
	CodePolicyNumberConflict = "POLICY_NUMBER_CONFLICT"
	// CodePatientIdentityConflict is R2: normalized patient mismatch.
	CodePatientIdentityConflict = "PATIENT_IDENTITY_CONFLICT"
	// CodeInvalidDateRange is R3: admission after discharge.
	CodeInvalidDateRange = "INVALID_DATE_RANGE"
	// CodePolicyNotActive is R4: policy active flag false.
	CodePolicyNotActive = "POLICY_NOT_ACTIVE"
	// CodeAmountReconciliationFailure is R5: bill lines != bill total.
	CodeAmountReconciliationFailure = "AMOUNT_RECONCILIATION_FAILURE"
	// CodeClaimExceedsBill is R6: claimed > bill total.
	CodeClaimExceedsBill = "CLAIM_EXCEEDS_BILL"
	// CodeDuplicateDocument is R7: repeated document at ingestion.
	CodeDuplicateDocument = "DUPLICATE_DOCUMENT"
	// CodeMissingRequiredDocument is R8: required doc absent.
	CodeMissingRequiredDocument = "MISSING_REQUIRED_DOCUMENT"
	// CodeExternalPolicyMismatch is R9: external policy check failed.
	CodeExternalPolicyMismatch = "EXTERNAL_POLICY_MISMATCH"
	// CodeDateConflict is R10: cross-source admission_date disagreement.
	CodeDateConflict = "DATE_CONFLICT"
)

// Required document keys for R8, in emission order.
const (
	// DocClaimForm is the claim form document type.
	DocClaimForm = "CLAIM_FORM"
	// DocDischargeSummary is the discharge summary document type.
	DocDischargeSummary = "DISCHARGE_SUMMARY"
	// DocHospitalBill is the hospital bill document type.
	DocHospitalBill = "HOSPITAL_BILL"
)

// requiredDocs fixes R8 emission order.
var requiredDocs = []string{DocClaimForm, DocDischargeSummary, DocHospitalBill}

// Exception is a single verification finding.
type Exception struct {
	Code        string
	Severity    string
	Message     string
	EvidenceIDs []string
}

// Input is the verification request. Zero values are meaningful:
// unset maps/slices behave as empty; Has* flags gate date/amount checks.
type Input struct {
	ClaimPolicyNumber string
	ClaimPatientName  string
	ClaimHospitalName string
	ClaimedPaise      int64
	Admission         time.Time
	Discharge         time.Time
	HasAdmission      bool
	HasDischarge      bool
	PolicyNumber      string
	PolicyPatient     string
	PolicyActive      bool
	DocsPresent       map[string]bool
	DocEvidence       map[string]map[string]string
	BillLinePaise     []int64
	BillTotalPaise    int64
	HasBillTotal      bool
	ExternalPolicyOK  bool
	ExternalMismatch  string
	Duplicates        []string
}

// Result is the verification outcome. Passed holds iff no exceptions.
type Result struct {
	Passed     bool
	Exceptions []Exception
}

// normalizeName trims, casefolds, and collapses interior whitespace,
// mirroring validation.normalizeField.
func normalizeName(v string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(v))), " ")
}

// normalizePolicy trims and casefolds a policy identifier.
func normalizePolicy(v string) string {
	return strings.ToUpper(strings.TrimSpace(v))
}

// fieldByName looks up a field case-insensitively within one doctype map.
func fieldByName(m map[string]string, name string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[name]; ok {
		return v
	}
	want := strings.ToLower(strings.TrimSpace(name))
	for k, v := range m {
		if strings.ToLower(strings.TrimSpace(k)) == want {
			return v
		}
	}
	return ""
}

// Verify runs rules R1-R10 in order and returns the combined result.
// Each rule appends at most one exception, except R7 (one per duplicate
// id) and R8 (one per missing required document).
func Verify(in Input) Result {
	var out []Exception

	// R1: policy identity.
	if normalizePolicy(in.ClaimPolicyNumber) != normalizePolicy(in.PolicyNumber) {
		out = append(out, Exception{
			Code:     CodePolicyNumberConflict,
			Severity: SeverityHigh,
			Message:  "Claim policy number does not match policy number.",
		})
	}

	// R2: patient identity across claim, policy, and per-document evidence.
	{
		distinct := make(map[string]struct{})
		if n := normalizeName(in.ClaimPatientName); n != "" {
			distinct[n] = struct{}{}
		}
		if n := normalizeName(in.PolicyPatient); n != "" {
			distinct[n] = struct{}{}
		}
		for _, fields := range in.DocEvidence {
			if n := normalizeName(fieldByName(fields, "patient_name")); n != "" {
				distinct[n] = struct{}{}
			}
		}
		if len(distinct) > 1 {
			out = append(out, Exception{
				Code:     CodePatientIdentityConflict,
				Severity: SeverityHigh,
				Message:  "Patient identity mismatch across sources.",
			})
		}
	}

	// R3: admission <= discharge when both set.
	if in.HasAdmission && in.HasDischarge && in.Admission.After(in.Discharge) {
		out = append(out, Exception{
			Code:     CodeInvalidDateRange,
			Severity: SeverityHigh,
			Message:  "Admission after discharge.",
		})
	}

	// R4: policy must be active.
	if !in.PolicyActive {
		out = append(out, Exception{
			Code:     CodePolicyNotActive,
			Severity: SeverityHigh,
			Message:  "Policy is not active.",
		})
	}

	// R5: bill lines reconcile to the bill total exactly in paise.
	if in.HasBillTotal && len(in.BillLinePaise) > 0 {
		var sum int64
		for _, line := range in.BillLinePaise {
			sum += line
		}
		if sum != in.BillTotalPaise {
			out = append(out, Exception{
				Code:     CodeAmountReconciliationFailure,
				Severity: SeverityHigh,
				Message:  fmt.Sprintf("Bill lines sum %d, total %d.", sum, in.BillTotalPaise),
			})
		}
	}

	// R6: claimed amount must not exceed the bill total.
	if in.HasBillTotal && in.ClaimedPaise > in.BillTotalPaise {
		out = append(out, Exception{
			Code:     CodeClaimExceedsBill,
			Severity: SeverityMedium,
			Message:  fmt.Sprintf("Claimed %d exceeds bill total %d.", in.ClaimedPaise, in.BillTotalPaise),
		})
	}

	// R7: duplicates handled at ingestion; one exception per doc id.
	for _, id := range in.Duplicates {
		k := strings.TrimSpace(id)
		if k == "" {
			continue
		}
		out = append(out, Exception{
			Code:        CodeDuplicateDocument,
			Severity:    SeverityMedium,
			Message:     fmt.Sprintf("Duplicate document: %s.", k),
			EvidenceIDs: []string{k},
		})
	}

	// R8: required documents present.
	for _, doc := range requiredDocs {
		if in.DocsPresent == nil || !in.DocsPresent[doc] {
			out = append(out, Exception{
				Code:     CodeMissingRequiredDocument,
				Severity: SeverityHigh,
				Message:  fmt.Sprintf("Missing required document: %s.", doc),
			})
		}
	}

	// R9: external policy check.
	if !in.ExternalPolicyOK {
		msg := "External policy check failed."
		if m := strings.TrimSpace(in.ExternalMismatch); m != "" {
			msg = fmt.Sprintf("External policy mismatch: %s.", m)
		}
		out = append(out, Exception{
			Code:     CodeExternalPolicyMismatch,
			Severity: SeverityHigh,
			Message:  msg,
		})
	}

	// R10: cross-source admission_date agreement across DocEvidence.
	{
		distinct := make(map[string]struct{})
		for _, fields := range in.DocEvidence {
			if v := strings.TrimSpace(fieldByName(fields, "admission_date")); v != "" {
				distinct[v] = struct{}{}
			}
		}
		if len(distinct) > 1 {
			out = append(out, Exception{
				Code:     CodeDateConflict,
				Severity: SeverityHigh,
				Message:  "Admission date conflict across sources.",
			})
		}
	}

	return Result{Passed: len(out) == 0, Exceptions: out}
}
