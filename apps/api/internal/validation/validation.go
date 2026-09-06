// Package validation implements the deterministic claim validation core.
//
// It is the Go port of packages/domain/validation.py and satisfies the
// behavioral contract in fixtures/golden_cases.json (cases 01-06).
// All functions are pure: no I/O, no clock reads, STDLIB only.
package validation

import (
	"fmt"
	"strings"
	"time"
)

// Severity is the outcome level of a validation rule.
type Severity string

const (
	// SeverityPass indicates the rule held.
	SeverityPass Severity = "PASS"
	// SeverityWarning indicates a non-blocking concern.
	SeverityWarning Severity = "WARNING"
	// SeverityFail indicates a blocking failure.
	SeverityFail Severity = "FAIL"
	// SeverityBlock indicates a hard block (e.g. policy not active).
	SeverityBlock Severity = "BLOCK"
)

// Rule ID constants mirroring the Python spec.
const (
	// RuleRequiredDocs requires CLAIM_FORM, HOSPITAL_FINAL_BILL, DISCHARGE_SUMMARY.
	RuleRequiredDocs = "REQUIRED_DOCS"
	// RulePolicyActive requires from <= incident <= to (when to applies).
	RulePolicyActive = "POLICY_ACTIVE"
	// RuleFieldConsistency requires normalized cross-source values to agree.
	RuleFieldConsistency = "FIELD_CONSISTENCY"
	// RuleDateLogic requires admission <= discharge.
	RuleDateLogic = "DATE_LOGIC"
	// RuleAmountReconciliation requires gross-discount == net in minor units.
	RuleAmountReconciliation = "AMOUNT_RECONCILIATION"
	// RuleDuplicateSHA256 rejects case-insensitive repeated document hashes.
	RuleDuplicateSHA256 = "DUPLICATE_SHA256"
)

// Result is the outcome of a single validation rule.
type Result struct {
	RuleID   string
	Passed   bool
	Severity Severity
	Message  string
}

func ok(ruleID, message string) Result {
	return Result{RuleID: ruleID, Passed: true, Severity: SeverityPass, Message: message}
}

func bad(ruleID, message string, severity Severity) Result {
	return Result{RuleID: ruleID, Passed: false, Severity: severity, Message: message}
}

// ValidateRequiredDocs reports which required document types are missing.
// Comparison is case-insensitive on trimmed values; blank entries are ignored.
func ValidateRequiredDocs(present, required []string) Result {
	set := make(map[string]struct{}, len(present))
	for _, p := range present {
		k := strings.ToUpper(strings.TrimSpace(p))
		if k == "" {
			continue
		}
		set[k] = struct{}{}
	}
	var missing []string
	for _, r := range required {
		k := strings.ToUpper(strings.TrimSpace(r))
		if k == "" {
			continue
		}
		if _, found := set[k]; !found {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return ok(RuleRequiredDocs, "All required documents present.")
	}
	return bad(RuleRequiredDocs, fmt.Sprintf("Missing: %s.", strings.Join(missing, ", ")), SeverityFail)
}

// ValidatePolicyActive holds iff from <= incident <= to (when hasTo).
// A zero incident or zero from, an incident before from, or an incident
// after to (when hasTo) yields a BLOCK failure.
func ValidatePolicyActive(incident, from time.Time, to time.Time, hasTo bool) Result {
	if incident.IsZero() || from.IsZero() {
		return bad(RulePolicyActive, "Incident/policy start missing.", SeverityBlock)
	}
	if incident.Before(from) {
		return bad(RulePolicyActive, "Incident precedes policy start.", SeverityBlock)
	}
	if hasTo && incident.After(to) {
		return bad(RulePolicyActive, "Incident after policy end.", SeverityBlock)
	}
	return ok(RulePolicyActive, "Policy active on incident date.")
}

// normalizeField trims, casefolds, and collapses interior whitespace.
func normalizeField(v string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(v))), " ")
}

// ValidateFieldConsistency holds iff every non-empty normalized value agrees.
func ValidateFieldConsistency(field string, valuesBySource map[string]string) Result {
	distinct := make(map[string]struct{})
	for _, v := range valuesBySource {
		n := normalizeField(v)
		if n == "" {
			continue
		}
		distinct[n] = struct{}{}
	}
	if len(distinct) == 0 {
		return bad(RuleFieldConsistency, fmt.Sprintf("'%s': no values.", field), SeverityFail)
	}
	if len(distinct) == 1 {
		return ok(RuleFieldConsistency, fmt.Sprintf("'%s' consistent.", field))
	}
	return bad(RuleFieldConsistency, fmt.Sprintf("'%s' mismatch.", field), SeverityFail)
}

// ValidateDateLogic holds iff both dates are present and admission <= discharge.
func ValidateDateLogic(admission, discharge time.Time, hasAdmission, hasDischarge bool) Result {
	if !hasAdmission || !hasDischarge {
		return bad(RuleDateLogic, "Admission/discharge missing.", SeverityFail)
	}
	if admission.After(discharge) {
		return bad(RuleDateLogic, "Admission after discharge.", SeverityFail)
	}
	return ok(RuleDateLogic, "Date logic holds.")
}

// ValidateAmountPaise holds iff gross-discount == net exactly in minor units.
// Negative inputs fail; only integer arithmetic is used.
func ValidateAmountPaise(gross, discount, net int64) Result {
	if gross < 0 || discount < 0 || net < 0 {
		return bad(RuleAmountReconciliation, "Negative amount.", SeverityFail)
	}
	if gross-discount == net {
		return ok(RuleAmountReconciliation, fmt.Sprintf("Reconciled: %d-%d==%d.", gross, discount, net))
	}
	return bad(RuleAmountReconciliation, fmt.Sprintf("Expected %d, got %d.", gross-discount, net), SeverityFail)
}

// ValidateDuplicateSHA256 fails on a case-insensitive repeat of a non-empty hash.
func ValidateDuplicateSHA256(hashes []string) Result {
	seen := make(map[string]struct{}, len(hashes))
	for _, h := range hashes {
		k := strings.ToLower(strings.TrimSpace(h))
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			return bad(RuleDuplicateSHA256, "Duplicate sha256.", SeverityFail)
		}
		seen[k] = struct{}{}
	}
	return ok(RuleDuplicateSHA256, "No duplicates.")
}
